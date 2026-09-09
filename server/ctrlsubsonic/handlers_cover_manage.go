package ctrlsubsonic

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"

	"github.com/jinzhu/gorm"

	"go.senan.xyz/gonic"
	"go.senan.xyz/gonic/covers"
	"go.senan.xyz/gonic/coversearch"
	"go.senan.xyz/gonic/db"
	"go.senan.xyz/gonic/server/ctrlsubsonic/params"
	"go.senan.xyz/gonic/server/ctrlsubsonic/spec"
	"go.senan.xyz/gonic/server/ctrlsubsonic/specid"
)

// cover art management is a gonic extension. all of it is admin only: an album's art is
// shared library state, so one user's change is everybody's change.

const (
	coverFetchTimeout = 20 * time.Second

	sourceURLGeneric = "url"
)

func (c *Controller) ServeGetCoverArtInfo(r *http.Request) *spec.Response {
	user := r.Context().Value(CtxUser).(*db.User)
	if !user.IsAdmin {
		return spec.NewError(50, "user not admin")
	}
	album, resp := c.coverManageAlbum(r)
	if resp != nil {
		return resp
	}
	sub := spec.NewResponse()
	sub.CoverArt = c.newCoverArtSpec(album)
	return sub
}

// ServeSearchCoverArt looks for candidate art across the configured sources. it's on
// chainRaw because it does remote i/o and the server's write timeout is 5s.
func (c *Controller) ServeSearchCoverArt(r *http.Request) *spec.Response {
	user := r.Context().Value(CtxUser).(*db.User)
	if !user.IsAdmin {
		return spec.NewError(50, "user not admin")
	}
	if c.coverSearcher == nil {
		return spec.NewError(0, "cover art search is not configured")
	}
	p := r.Context().Value(CtxParams).(params.Params)

	var query coversearch.Query
	query.Text = p.GetOr("query", "")
	query.Limit = p.GetOrInt("limit", 0)
	if sources := p.GetOr("sources", ""); sources != "" {
		query.Sources = strings.Split(sources, ",")
	}

	// an id is optional: without one this is a free text search. with one we can prefill
	// from the album's own tags and, better, use its musicbrainz id for an exact hit
	if _, err := p.Get("id"); err == nil {
		album, resp := c.coverManageAlbum(r)
		if resp != nil {
			return resp
		}
		query.Album = album.TagTitle
		query.Artist = album.TagAlbumArtist
		query.MusicBrainzID = album.TagBrainzID
	}

	results, err := c.coverSearcher.Search(r.Context(), query)
	if err != nil {
		return spec.NewError(0, "search cover art: %v", err)
	}

	sub := spec.NewResponse()
	sub.CoverArtSearch = &spec.CoverArtSearch{List: []*spec.CoverArtSearchResult{}}
	for _, result := range results {
		sub.CoverArtSearch.List = append(sub.CoverArtSearch.List, &spec.CoverArtSearchResult{
			Source:       result.Source,
			URL:          result.URL,
			ThumbnailURL: result.ThumbnailURL,
			Title:        result.Title,
			Artist:       result.Artist,
			Width:        result.Width,
			Height:       result.Height,
		})
	}
	return sub
}

func (c *Controller) ServeDeleteCoverArt(r *http.Request) *spec.Response {
	user := r.Context().Value(CtxUser).(*db.User)
	if !user.IsAdmin {
		return spec.NewError(50, "user not admin")
	}
	album, resp := c.coverManageAlbum(r)
	if resp != nil {
		return resp
	}
	if err := c.clearAlbumCoverOverride(album); err != nil {
		return spec.NewError(0, "clear cover override: %v", err)
	}
	sub := spec.NewResponse()
	sub.CoverArt = c.newCoverArtSpec(album)
	return sub
}

// ServeSetCoverArt sets an album's cover from a url or from a track's embedded art. it's on
// chainRaw because the url arm does a remote fetch and the server's write timeout is 5s.
func (c *Controller) ServeSetCoverArt(r *http.Request) *spec.Response {
	user := r.Context().Value(CtxUser).(*db.User)
	if !user.IsAdmin {
		return spec.NewError(50, "user not admin")
	}
	p := r.Context().Value(CtxParams).(params.Params)
	album, resp := c.coverManageAlbum(r)
	if resp != nil {
		return resp
	}

	sourceURL, hasURL := p.Get("url")
	trackID, hasTrack := p.GetID("trackId")
	switch {
	case hasURL == nil && hasTrack == nil:
		return spec.NewError(10, "please provide only one of `url` or `trackId`")
	case hasURL == nil:
		cover, err := c.coverFromURL(r.Context(), sourceURL)
		if err != nil {
			return spec.NewError(0, "fetch cover from url: %v", err)
		}
		if err := c.setAlbumCoverOverride(album, cover, sourceOf(sourceURL), sourceURL, user); err != nil {
			return spec.NewError(0, "set cover override: %v", err)
		}
	case hasTrack == nil:
		cover, err := c.coverFromTrack(album, trackID)
		if err != nil {
			return spec.NewError(0, "read embedded cover: %v", err)
		}
		if err := c.setAlbumCoverOverride(album, cover, "embedded", trackID.String(), user); err != nil {
			return spec.NewError(0, "set cover override: %v", err)
		}
	default:
		return spec.NewError(10, "please provide a `url` or `trackId` parameter")
	}

	sub := spec.NewResponse()
	sub.CoverArt = c.newCoverArtSpec(album)
	return sub
}

// ServeUploadCoverArt takes a multipart body. note that params.New snapshots r.Form before
// any multipart parse, so multipart *fields* are invisible to the params api: every scalar
// (u, t, s, p, c, v, f, id) has to arrive in the query string, and only the file comes from
// the body. it's on chainRaw so the 5s read timeout doesn't truncate the upload.
func (c *Controller) ServeUploadCoverArt(w http.ResponseWriter, r *http.Request) *spec.Response {
	user := r.Context().Value(CtxUser).(*db.User)
	if !user.IsAdmin {
		return spec.NewError(50, "user not admin")
	}
	album, resp := c.coverManageAlbum(r)
	if resp != nil {
		return resp
	}

	r.Body = http.MaxBytesReader(w, r.Body, covers.MaxBytes)
	// body is already capped by MaxBytesReader above
	if err := r.ParseMultipartForm(4 << 20); err != nil { //nolint:gosec // G120: bounded by MaxBytesReader
		return spec.NewError(0, "parse upload: %v", err)
	}
	defer func() { _ = r.MultipartForm.RemoveAll() }()

	file, header, err := r.FormFile("file")
	if err != nil {
		return spec.NewError(10, "please provide a `file` part: %v", err)
	}
	defer file.Close()

	cover, err := c.coverStore.Put(file)
	if err != nil {
		return spec.NewError(0, "store cover: %v", err)
	}
	if err := c.setAlbumCoverOverride(album, cover, "upload", header.Filename, user); err != nil {
		return spec.NewError(0, "set cover override: %v", err)
	}

	sub := spec.NewResponse()
	sub.CoverArt = c.newCoverArtSpec(album)
	return sub
}

func (c *Controller) coverManageAlbum(r *http.Request) (*db.Album, *spec.Response) {
	p := r.Context().Value(CtxParams).(params.Params)
	id, err := p.GetID("id")
	if err != nil {
		return nil, spec.NewError(10, "please provide an `id` parameter")
	}
	if id.Type != specid.Album {
		return nil, spec.NewError(10, "please provide an album id, got %q", id.String())
	}
	var album db.Album
	if err := c.dbc.Where("id=?", id.Value).First(&album).Error; err != nil {
		return nil, spec.NewError(70, "album with id %q not found", id.String())
	}
	return &album, nil
}

func (c *Controller) coverFromURL(ctx context.Context, raw string) (covers.Cover, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return covers.Cover{}, fmt.Errorf("parse url: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return covers.Cover{}, fmt.Errorf("unsupported url scheme %q", parsed.Scheme)
	}

	ctx, cancel := context.WithTimeout(ctx, coverFetchTimeout)
	defer cancel()

	// G704: the scheme is checked above and coverFetchClient refuses to connect to
	// anything but a public address, so this can't be aimed at the local network
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil) //nolint:gosec
	if err != nil {
		return covers.Cover{}, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", fmt.Sprintf("%s/%s", gonic.Name, gonic.Version))

	resp, err := c.httpClientForCoverFetch().Do(req) //nolint:gosec // see above
	if err != nil {
		return covers.Cover{}, fmt.Errorf("request cover: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return covers.Cover{}, fmt.Errorf("request cover: bad status %d", resp.StatusCode)
	}
	return c.coverStore.Put(io.LimitReader(resp.Body, covers.MaxBytes+1))
}

func (c *Controller) coverFromTrack(album *db.Album, trackID specid.ID) (covers.Cover, error) {
	if trackID.Type != specid.Track {
		return covers.Cover{}, fmt.Errorf("not a track id: %q", trackID.String())
	}
	var track db.Track
	if err := c.dbc.Preload("Album").Where("id=?", trackID.Value).First(&track).Error; err != nil {
		return covers.Cover{}, fmt.Errorf("find track: %w", err)
	}
	if track.AlbumID != album.ID {
		return covers.Cover{}, errors.New("track does not belong to that album")
	}
	body, err := coverForTrack(c.dbc, c.tagReader, trackID.Value)
	if err != nil {
		return covers.Cover{}, fmt.Errorf("read cover from track: %w", err)
	}
	defer body.Close()
	return c.coverStore.Put(body)
}

// setAlbumCoverOverride records the override and republishes it onto the album row that the
// read path consults, then drops any cached resizes of the old art.
func (c *Controller) setAlbumCoverOverride(album *db.Album, cover covers.Cover, source, sourceURL string, user *db.User) error {
	var oldHash, oldExt string

	err := c.dbc.Transaction(func(tx *db.DB) error {
		var override db.AlbumCoverOverride
		err := tx.Where("root_dir=? AND left_path=? AND right_path=?", album.RootDir, album.LeftPath, album.RightPath).
			First(&override).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return fmt.Errorf("find existing override: %w", err)
		}
		oldHash, oldExt = override.Hash, override.Ext

		override.AlbumID = &album.ID
		override.RootDir, override.LeftPath, override.RightPath = album.RootDir, album.LeftPath, album.RightPath
		override.Hash, override.Ext, override.MIME = cover.Hash, cover.Ext, cover.MIME
		override.Width, override.Height, override.Size = cover.Width, cover.Height, cover.Size
		override.Source, override.SourceURL = source, sourceURL
		override.UpdatedAt = time.Now()
		override.UpdatedByUserID = &user.ID

		if err := tx.Save(&override).Error; err != nil {
			return fmt.Errorf("save override: %w", err)
		}
		return tx.Model(db.Album{}).Where("id=?", album.ID).
			Updates(map[string]any{"cover_override_hash": cover.Hash, "cover_override_ext": cover.Ext}).Error
	})
	if err != nil {
		return err
	}

	album.CoverOverrideHash, album.CoverOverrideExt = cover.Hash, cover.Ext
	c.dropUnreferencedCover(oldHash, oldExt, cover.Hash)
	return c.purgeAlbumCovers(album)
}

func (c *Controller) clearAlbumCoverOverride(album *db.Album) error {
	var oldHash, oldExt string

	err := c.dbc.Transaction(func(tx *db.DB) error {
		var override db.AlbumCoverOverride
		err := tx.Where("root_dir=? AND left_path=? AND right_path=?", album.RootDir, album.LeftPath, album.RightPath).
			First(&override).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("find existing override: %w", err)
		}
		oldHash, oldExt = override.Hash, override.Ext

		if err := tx.Delete(&override).Error; err != nil {
			return fmt.Errorf("delete override: %w", err)
		}
		return tx.Model(db.Album{}).Where("id=?", album.ID).
			Updates(map[string]any{"cover_override_hash": "", "cover_override_ext": ""}).Error
	})
	if err != nil {
		return err
	}

	album.CoverOverrideHash, album.CoverOverrideExt = "", ""
	c.dropUnreferencedCover(oldHash, oldExt, "")
	return c.purgeAlbumCovers(album)
}

// dropUnreferencedCover deletes a stored blob only once nothing points at it. two albums can
// legitimately share a hash, so this can't be a plain delete.
func (c *Controller) dropUnreferencedCover(hash, ext, keep string) {
	if hash == "" || hash == keep {
		return
	}
	var count int
	if err := c.dbc.Model(db.AlbumCoverOverride{}).Where("hash=?", hash).Count(&count).Error; err != nil {
		log.Printf("error counting cover references for %q: %v", hash, err)
		return
	}
	if count > 0 {
		return
	}
	if err := c.coverStore.Delete(hash, ext); err != nil {
		log.Printf("error deleting unreferenced cover %q: %v", hash, err)
	}
}

func (c *Controller) purgeAlbumCovers(album *db.Album) error {
	ids := []specid.ID{{Type: specid.Album, Value: album.ID}}
	// a collection's mosaic is built from its albums' art, so it goes stale too
	var collectionIDs []int
	if err := c.dbc.Model(db.CollectionAlbum{}).Where("album_id=?", album.ID).
		Pluck("DISTINCT collection_id", &collectionIDs).Error; err != nil {
		return fmt.Errorf("find collections containing album: %w", err)
	}
	for _, id := range collectionIDs {
		ids = append(ids, specid.ID{Type: specid.Collection, Value: id})
	}
	return purgeCachedCovers(c.coverCache, ids...)
}

func (c *Controller) newCoverArtSpec(album *db.Album) *spec.CoverArt {
	ret := &spec.CoverArt{
		ID:         *album.SID(),
		CoverID:    album.CoverSID(),
		Token:      spec.CoverArtToken(album),
		Overridden: album.CoverOverrideHash != "",
	}
	if !ret.Overridden {
		return ret
	}
	var override db.AlbumCoverOverride
	if err := c.dbc.Where("album_id=?", album.ID).First(&override).Error; err != nil {
		return ret
	}
	ret.Source, ret.SourceURL = override.Source, override.SourceURL
	ret.Width, ret.Height, ret.Size = override.Width, override.Height, override.Size
	ret.UpdatedAt = &override.UpdatedAt
	if override.UpdatedByUserID != nil {
		var user db.User
		if err := c.dbc.Where("id=?", *override.UpdatedByUserID).First(&user).Error; err == nil {
			ret.UpdatedBy = user.Name
		}
	}
	return ret
}

func sourceOf(raw string) string {
	switch {
	case strings.Contains(raw, "coverartarchive.org"), strings.Contains(raw, "archive.org"):
		return "coverartarchive"
	case strings.Contains(raw, "dzcdn.net"), strings.Contains(raw, "deezer.com"):
		return "deezer"
	case strings.Contains(raw, "mzstatic.com"), strings.Contains(raw, "apple.com"):
		return "itunes"
	}
	return sourceURLGeneric
}

// coverFetchClient refuses to connect to loopback, link local or private addresses. an
// admin pasting a url is a lower bar than a random user, but it's still a url the server
// dials on their behalf, so don't let it be pointed at the host's own network. the check
// is in the dialer rather than on the parsed host so that a dns name resolving to a
// private address, or a redirect to one, is caught too.
//
//nolint:gochecknoglobals // one shared client, same as http.DefaultClient
var coverFetchClient = &http.Client{
	Timeout: coverFetchTimeout,
	Transport: &http.Transport{
		DialContext: (&net.Dialer{Timeout: 10 * time.Second, Control: refusePrivateAddr}).DialContext,
	},
}

// httpClientForCoverFetch lets tests point the url arm at a loopback httptest server,
// which the guarded default deliberately refuses.
func (c *Controller) httpClientForCoverFetch() *http.Client {
	if c.coverFetchClient != nil {
		return c.coverFetchClient
	}
	return coverFetchClient
}

func refusePrivateAddr(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("split dial address: %w", err)
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("parse dial address: %w", err)
	}
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return fmt.Errorf("refusing to fetch from non public address %s", ip)
	}
	return nil
}
