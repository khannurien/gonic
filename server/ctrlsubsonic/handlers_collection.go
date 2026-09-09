package ctrlsubsonic

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jinzhu/gorm"

	"go.senan.xyz/gonic/collection"
	"go.senan.xyz/gonic/db"
	"go.senan.xyz/gonic/server/ctrlsubsonic/params"
	"go.senan.xyz/gonic/server/ctrlsubsonic/spec"
	"go.senan.xyz/gonic/server/ctrlsubsonic/specid"
)

// collections are a gonic extension: ordered lists of whole albums. read access follows the
// playlist rule (your own, plus other people's public ones), write access is owner only.

func (c *Controller) ServeGetCollections(r *http.Request) *spec.Response {
	user := r.Context().Value(CtxUser).(*db.User)

	var collections []*db.Collection
	if err := c.dbc.
		Scopes(collection.Visible(user.ID)).
		Preload("User").
		Order("collections.name COLLATE NOCASE").
		Find(&collections).Error; err != nil {
		return spec.NewError(0, "find collections: %v", err)
	}

	sub := spec.NewResponse()
	sub.Collections = &spec.Collections{List: []*spec.Collection{}}
	for _, coll := range collections {
		rendered, err := c.collectionRender(coll, false)
		if err != nil {
			return spec.NewError(0, "render collection: %v", err)
		}
		sub.Collections.List = append(sub.Collections.List, rendered)
	}
	return sub
}

func (c *Controller) ServeGetCollection(r *http.Request) *spec.Response {
	user := r.Context().Value(CtxUser).(*db.User)
	p := r.Context().Value(CtxParams).(params.Params)

	coll, resp := c.collectionForRead(r)
	if resp != nil {
		return resp
	}

	rendered, err := c.collectionRender(coll, true)
	if err != nil {
		return spec.NewError(0, "render collection: %v", err)
	}
	if err := c.collectionAttachContents(rendered, coll, user, p); err != nil {
		return spec.NewError(0, "load collection contents: %v", err)
	}

	sub := spec.NewResponse()
	sub.Collection = rendered
	return sub
}

func (c *Controller) ServeCreateCollection(r *http.Request) *spec.Response {
	user := r.Context().Value(CtxUser).(*db.User)
	p := r.Context().Value(CtxParams).(params.Params)

	name, err := p.Get("name")
	if err != nil {
		return spec.NewError(10, "please provide a `name` parameter")
	}

	// parse before anything is written, otherwise a bad albumId leaves an empty
	// collection behind for a call the client saw fail
	albumIDs, err := collectionAlbumIDs(p, "albumId")
	if err != nil {
		return spec.NewError(10, "please provide valid album ids: %v", err)
	}

	now := time.Now()
	coll := db.Collection{
		UserID:    user.ID,
		Name:      name,
		Comment:   p.GetOr("comment", ""),
		IsPublic:  p.GetOrBool("public", false),
		CreatedAt: now,
		UpdatedAt: now,
	}
	err = c.dbc.Transaction(func(tx *db.DB) error {
		if err := tx.Save(&coll).Error; err != nil {
			return fmt.Errorf("save collection: %w", err)
		}
		return collection.SetAlbums(tx, coll.ID, albumIDs)
	})
	if err != nil {
		return spec.NewError(0, "create collection: %v", err)
	}

	rendered, err := c.collectionRender(&coll, false)
	if err != nil {
		return spec.NewError(0, "render collection: %v", err)
	}
	sub := spec.NewResponse()
	sub.Collection = rendered
	return sub
}

func (c *Controller) ServeUpdateCollection(r *http.Request) *spec.Response {
	p := r.Context().Value(CtxParams).(params.Params)

	coll, resp := c.collectionForWrite(r)
	if resp != nil {
		return resp
	}

	if val, err := p.Get("name"); err == nil {
		coll.Name = val
	}
	if val, err := p.Get("comment"); err == nil {
		coll.Comment = val
	}
	if val, err := p.GetBool("public"); err == nil {
		coll.IsPublic = val
	}
	coll.UpdatedAt = time.Now()

	if err := c.dbc.Save(coll).Error; err != nil {
		return spec.NewError(0, "update collection: %v", err)
	}

	// a full ordered replace, which covers add, remove and reorder in one call. subsonic
	// has no reorder primitive to model this on, so don't invent a partial one
	if _, err := p.Get("albumId"); err == nil {
		albumIDs, err := collectionAlbumIDs(p, "albumId")
		if err != nil {
			return spec.NewError(10, "please provide valid album ids: %v", err)
		}
		if err := collection.SetAlbums(c.dbc, coll.ID, albumIDs); err != nil {
			return spec.NewError(0, "set collection albums: %v", err)
		}
		if err := purgeCachedCovers(c.coverCache, *coll.SID()); err != nil {
			return spec.NewError(0, "purge collection cover: %v", err)
		}
	}

	return spec.NewResponse()
}

func (c *Controller) ServeDeleteCollection(r *http.Request) *spec.Response {
	coll, resp := c.collectionForWrite(r)
	if resp != nil {
		return resp
	}
	if err := c.dbc.Delete(coll).Error; err != nil {
		return spec.NewError(0, "delete collection: %v", err)
	}
	if err := purgeCachedCovers(c.coverCache, *coll.SID()); err != nil {
		return spec.NewError(0, "purge collection cover: %v", err)
	}
	return spec.NewResponse()
}

func (c *Controller) collectionForRead(r *http.Request) (*db.Collection, *spec.Response) {
	p := r.Context().Value(CtxParams).(params.Params)

	id, err := p.GetID("id")
	if err != nil {
		return nil, spec.NewError(10, "please provide an `id` parameter")
	}
	return c.collectionForReadID(r, id)
}

// collectionForReadID is collectionForRead for a caller that already resolved the id from
// somewhere other than `id`, eg. getPlaylist's `playlistId`.
func (c *Controller) collectionForReadID(r *http.Request, id specid.ID) (*db.Collection, *spec.Response) {
	user := r.Context().Value(CtxUser).(*db.User)

	coll, err := c.collectionByID(id)
	if err != nil {
		return nil, spec.NewError(70, "collection with id %q not found", id.String())
	}
	if coll.UserID != user.ID && !coll.IsPublic {
		return nil, spec.NewError(50, "you aren't allowed to read that user's collection")
	}
	return coll, nil
}

func (c *Controller) collectionForWrite(r *http.Request) (*db.Collection, *spec.Response) {
	user := r.Context().Value(CtxUser).(*db.User)
	coll, resp := c.collectionForRead(r)
	if resp != nil {
		return nil, resp
	}
	if coll.UserID != user.ID {
		return nil, spec.NewError(50, "you aren't allowed to change that user's collection")
	}
	return coll, nil
}

func (c *Controller) collectionByID(id specid.ID) (*db.Collection, error) {
	if id.Type != specid.Collection {
		return nil, fmt.Errorf("not a collection id: %q", id.String())
	}
	var coll db.Collection
	if err := c.dbc.Preload("User").Where("id=?", id.Value).First(&coll).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, collection.ErrNotFound
		}
		return nil, fmt.Errorf("find collection: %w", err)
	}
	return &coll, nil
}

// collectionRender builds the header. counts are derived rather than stored so they can
// never drift from the library.
func (c *Controller) collectionRender(coll *db.Collection, withAlbums bool) (*spec.Collection, error) {
	albumIDs, err := collection.AlbumIDs(c.dbc, coll.ID)
	if err != nil {
		return nil, err
	}
	trackCount, duration, err := collection.Stats(c.dbc, coll.ID)
	if err != nil {
		return nil, err
	}

	ret := &spec.Collection{
		ID:         *coll.SID(),
		Name:       coll.Name,
		Comment:    coll.Comment,
		Public:     coll.IsPublic,
		AlbumCount: len(albumIDs),
		SongCount:  trackCount,
		Duration:   duration,
		Created:    coll.CreatedAt,
		Changed:    coll.UpdatedAt,
	}
	if coll.User != nil {
		ret.Owner = coll.User.Name
	}
	if len(albumIDs) > 0 {
		ret.CoverID = coll.SID()
	}
	if !withAlbums {
		return ret, nil
	}
	return ret, nil
}

// collectionAttachContents fills in the album objects and the track expansion.
func (c *Controller) collectionAttachContents(ret *spec.Collection, coll *db.Collection, user *db.User, p params.Params) error {
	albumIDs, err := collection.AlbumIDs(c.dbc, coll.ID)
	if err != nil {
		return err
	}

	ret.List = []*spec.Album{}
	if len(albumIDs) > 0 {
		var albums []*spec.AlbumRow
		if err := c.dbc.
			Scopes(spec.LoadAlbumByTags(user.ID)).
			Where("albums.id IN (?)", albumIDs).
			Find(&albums).Error; err != nil {
			return fmt.Errorf("find collection albums: %w", err)
		}
		byID := make(map[int]*spec.AlbumRow, len(albums))
		for _, album := range albums {
			byID[album.ID] = album
		}
		// keep the collection's own order, which the IN () query doesn't preserve
		for _, id := range albumIDs {
			if album, ok := byID[id]; ok {
				ret.List = append(ret.List, spec.NewAlbumByTags(album, album.Credits))
			}
		}
	}

	var tracks []*spec.TrackRow
	if err := c.dbc.
		Scopes(spec.LoadTrackByFolder(user.ID), spec.CollectionTracks(coll.ID)).
		Find(&tracks).Error; err != nil {
		return fmt.Errorf("find collection tracks: %w", err)
	}

	client := p.GetOr("c", "")
	transcodeMeta := streamGetTranscodeMeta(c.dbc, user.ID, client)

	ret.Tracks = make([]*spec.TrackChild, 0, len(tracks))
	for _, track := range tracks {
		child := spec.NewTCTrackByFolder(track, track.Album)
		child.TranscodeMeta = transcodeMeta
		ret.Tracks = append(ret.Tracks, child)
	}
	return nil
}

// collectionAlbumIDs parses the repeated albumId param. values are read as strings rather
// than through GetIDList so that an explicitly empty one can mean "no albums", which is the
// only way a client has to empty a collection.
func collectionAlbumIDs(p params.Params, key string) ([]int, error) {
	values, err := p.GetList(key)
	if err != nil && !errors.Is(err, params.ErrNoValues) {
		return nil, err
	}
	out := make([]int, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		id, err := specid.New(value)
		if err != nil {
			return nil, fmt.Errorf("parse album id %q: %w", value, err)
		}
		if id.Type != specid.Album {
			return nil, fmt.Errorf("not an album id: %q", id.String())
		}
		out = append(out, id.Value)
	}
	return out, nil
}
