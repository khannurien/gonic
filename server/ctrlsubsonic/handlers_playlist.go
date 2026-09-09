package ctrlsubsonic

import (
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"time"

	"go.senan.xyz/gonic/collection"
	"go.senan.xyz/gonic/db"
	playlistp "go.senan.xyz/gonic/playlist"
	paramsp "go.senan.xyz/gonic/server/ctrlsubsonic/params"
	"go.senan.xyz/gonic/server/ctrlsubsonic/spec"
	"go.senan.xyz/gonic/server/ctrlsubsonic/specid"
	"go.senan.xyz/gonic/server/ctrlsubsonic/specidpaths"
)

func (c *Controller) ServeGetPlaylists(r *http.Request) *spec.Response {
	params := r.Context().Value(CtxParams).(paramsp.Params)
	user := r.Context().Value(CtxUser).(*db.User)
	paths, err := c.playlistStore.List()
	if err != nil {
		return spec.NewError(0, "error listing playlists: %v", err)
	}
	sub := spec.NewResponse()
	sub.Playlists = &spec.Playlists{
		List: []*spec.Playlist{},
	}
	for _, path := range paths {
		playlist, err := c.playlistStore.Read(path)
		if err != nil {
			return spec.NewError(0, "error reading playlist %q: %v", path, err)
		}
		if playlist.UserID != user.ID && !playlist.IsPublic {
			continue
		}
		playlistID := playlistIDEncode(path)
		rendered, err := playlistRender(c, params, playlist, playlistID, false)
		if err != nil {
			return spec.NewError(0, "error rendering playlist %q: %v", path, err)
		}
		sub.Playlists.List = append(sub.Playlists.List, rendered)
	}

	// collections are gonic's own concept, but a client that only speaks vanilla subsonic
	// should still be able to play one. mirror each visible collection as a read only
	// track playlist, synthesised here rather than written out as an m3u
	mirrored, err := c.collectionsAsPlaylists(user, params)
	if err != nil {
		return spec.NewError(0, "render collections as playlists: %v", err)
	}
	sub.Playlists.List = append(sub.Playlists.List, mirrored...)

	return sub
}

// collectionsAsPlaylists renders every collection the user may see as a read only playlist.
// the ids stay native (co-N): playlist ids are opaque strings in subsonic, and dispatching
// on id.Type is a total switch with no chance of colliding with a real m3u path.
func (c *Controller) collectionsAsPlaylists(user *db.User, params paramsp.Params) ([]*spec.Playlist, error) {
	var collections []*db.Collection
	if err := c.dbc.
		Scopes(collection.Visible(user.ID)).
		Preload("User").
		Order("collections.name COLLATE NOCASE").
		Find(&collections).Error; err != nil {
		return nil, fmt.Errorf("find collections: %w", err)
	}

	ret := make([]*spec.Playlist, 0, len(collections))
	for _, coll := range collections {
		rendered, err := c.collectionAsPlaylist(coll, user, params, false)
		if err != nil {
			return nil, err
		}
		ret = append(ret, rendered)
	}
	return ret, nil
}

func (c *Controller) collectionAsPlaylist(coll *db.Collection, user *db.User, params paramsp.Params, withItems bool) (*spec.Playlist, error) {
	trackCount, duration, err := collection.Stats(c.dbc, coll.ID)
	if err != nil {
		return nil, err
	}

	resp := &spec.Playlist{
		ID:        *coll.SID(),
		Name:      coll.Name,
		Comment:   coll.Comment,
		Created:   coll.CreatedAt,
		Changed:   coll.UpdatedAt,
		SongCount: trackCount,
		Duration:  duration,
		Public:    coll.IsPublic,
	}
	if coll.User != nil {
		resp.Owner = coll.User.Name
	}
	if trackCount > 0 {
		resp.CoverID = coll.SID()
	}
	if !withItems {
		return resp, nil
	}

	var tracks []*spec.TrackRow
	if err := c.dbc.
		Scopes(spec.LoadTrackByFolder(user.ID), spec.CollectionTracks(coll.ID)).
		Find(&tracks).Error; err != nil {
		return nil, fmt.Errorf("find collection tracks: %w", err)
	}

	transcodeMeta := streamGetTranscodeMeta(c.dbc, user.ID, params.GetOr("c", ""))
	resp.List = make([]*spec.TrackChild, 0, len(tracks))
	for _, track := range tracks {
		child := spec.NewTCTrackByFolder(track, track.Album)
		child.TranscodeMeta = transcodeMeta
		resp.List = append(resp.List, child)
	}
	resp.SongCount = len(resp.List)
	return resp, nil
}

// errCollectionNotAPlaylist is what a vanilla client gets when it tries to edit a mirrored
// collection. a track level edit has no sensible meaning against an ordered album list, so
// refusing cleanly beats half supporting it.
func errCollectionNotAPlaylist(verb string) *spec.Response {
	return spec.NewError(0, "this playlist is a collection, %s it with %sCollection instead", verb, verb)
}

func (c *Controller) ServeGetPlaylist(r *http.Request) *spec.Response {
	user := r.Context().Value(CtxUser).(*db.User)
	params := r.Context().Value(CtxParams).(paramsp.Params)
	playlistID, err := params.GetFirstID("id", "playlistId")
	if err != nil {
		return spec.NewError(10, "please provide an `id` parameter")
	}
	if playlistID.Type == specid.Collection {
		// by id, not by re-reading `id`: this handler also accepts `playlistId`
		coll, resp := c.collectionForReadID(r, playlistID)
		if resp != nil {
			return resp
		}
		rendered, err := c.collectionAsPlaylist(coll, user, params, true)
		if err != nil {
			return spec.NewError(0, "render collection as playlist: %v", err)
		}
		sub := spec.NewResponse()
		sub.Playlist = rendered
		return sub
	}

	playlist, err := c.playlistStore.Read(playlistIDDecode(playlistID))
	if err != nil {
		return spec.NewError(70, "playlist with id %s not found", playlistID)
	}
	if playlist.UserID != user.ID && !playlist.IsPublic {
		return spec.NewError(50, "you aren't allowed to read that user's playlist")
	}
	sub := spec.NewResponse()
	rendered, err := playlistRender(c, params, playlist, playlistID, true)
	if err != nil {
		return spec.NewError(0, "error rendering playlist: %v", err)
	}
	sub.Playlist = rendered
	return sub
}

func (c *Controller) ServeCreateOrUpdatePlaylist(r *http.Request) *spec.Response {
	user := r.Context().Value(CtxUser).(*db.User)
	params := r.Context().Value(CtxParams).(paramsp.Params)

	playlistID, _ := params.GetFirstID("id", "playlistId")
	if playlistID.Type == specid.Collection {
		return errCollectionNotAPlaylist("update")
	}
	playlistPath := playlistIDDecode(playlistID)

	var playlist playlistp.Playlist
	if playlistPath != "" {
		if pl, err := c.playlistStore.Read(playlistPath); err == nil && pl != nil {
			playlist = *pl
		}
	}

	if playlist.UserID != 0 && playlist.UserID != user.ID {
		return spec.NewError(50, "you aren't allowed update that user's playlist")
	}

	// path not found, make sure we don't use caller's provided path since it might be in another user's dir
	if playlist.UserID == 0 {
		playlistPath = playlistp.NewPath(user.ID, fmt.Sprint(time.Now().UnixMilli()))
		playlistID = playlistIDEncode(playlistPath)
	}

	playlist.UserID = user.ID
	playlist.UpdatedAt = time.Now()

	if val, err := params.Get("name"); err == nil {
		playlist.Name = val
	}

	playlist.Items = nil
	ids, err := params.GetIDList("songId")
	if err != nil && !errors.Is(err, paramsp.ErrNoValues) {
		return spec.NewError(10, "please provide valid song ids: %v", err)
	}
	for _, id := range ids {
		r, err := specidpaths.Locate(c.dbc, id)
		if err != nil {
			return spec.NewError(70, "couldn't find a track with id %v: %v", id, err)
		}
		playlist.Items = append(playlist.Items, r.AbsPath())
	}

	if err := c.playlistStore.Write(playlistPath, &playlist); err != nil {
		return spec.NewError(0, "save playlist: %v", err)
	}

	sub := spec.NewResponse()
	rendered, err := playlistRender(c, params, &playlist, playlistID, true)
	if err != nil {
		return spec.NewError(0, "error rendering playlist: %v", err)
	}
	sub.Playlist = rendered
	return sub
}

func (c *Controller) ServeUpdatePlaylist(r *http.Request) *spec.Response {
	user := r.Context().Value(CtxUser).(*db.User)
	p := r.Context().Value(CtxParams).(paramsp.Params)

	playlistID, err := p.GetFirstID("id", "playlistId")
	if err != nil {
		return spec.NewError(10, "please provide an `id` or `playlistId` parameter")
	}
	if playlistID.Type == specid.Collection {
		return errCollectionNotAPlaylist("update")
	}
	playlistPath := playlistIDDecode(playlistID)
	playlist, err := c.playlistStore.Read(playlistPath)
	if err != nil {
		return spec.NewError(0, "find playlist: %v", err)
	}

	// update meta info
	if playlist.UserID != 0 && playlist.UserID != user.ID {
		return spec.NewResponse()
	}

	if val, err := p.Get("name"); err == nil {
		playlist.Name = val
	}
	if val, err := p.Get("comment"); err == nil {
		playlist.Comment = val
	}
	if val, err := p.GetBool("public"); err == nil {
		playlist.IsPublic = val
	}

	// delete items
	if indexes, err := p.GetIntList("songIndexToRemove"); err == nil {
		sort.Sort(sort.Reverse(sort.IntSlice(indexes)))
		for _, i := range indexes {
			playlist.Items = append(playlist.Items[:i], playlist.Items[i+1:]...)
		}
	}

	// add items
	ids, err := p.GetIDList("songIdToAdd")
	if err != nil && !errors.Is(err, paramsp.ErrNoValues) {
		return spec.NewError(10, "please provide valid song ids: %v", err)
	}
	for _, id := range ids {
		item, err := specidpaths.Locate(c.dbc, id)
		if err != nil {
			return spec.NewError(70, "couldn't find a track with id %v: %v", id, err)
		}
		playlist.Items = append(playlist.Items, item.AbsPath())
	}

	if err := c.playlistStore.Write(playlistPath, playlist); err != nil {
		return spec.NewError(0, "save playlist: %v", err)
	}
	return spec.NewResponse()
}

func (c *Controller) ServeDeletePlaylist(r *http.Request) *spec.Response {
	user := r.Context().Value(CtxUser).(*db.User)
	params := r.Context().Value(CtxParams).(paramsp.Params)
	playlistID, err := params.GetFirstID("id", "playlistId")
	if err != nil {
		return spec.NewError(10, "please provide an `id` or `playlistId` parameter")
	}
	if playlistID.Type == specid.Collection {
		return errCollectionNotAPlaylist("delete")
	}
	playlistPath := playlistIDDecode(playlistID)
	playlist, err := c.playlistStore.Read(playlistPath)
	if err != nil {
		return spec.NewError(70, "playlist with id %s not found", playlistID)
	}
	if playlist.UserID != 0 && playlist.UserID != user.ID {
		return spec.NewError(50, "you aren't allowed to delete that user's playlist")
	}
	if err := c.playlistStore.Delete(playlistPath); err != nil {
		return spec.NewError(0, "delete playlist: %v", err)
	}
	return spec.NewResponse()
}

func playlistIDEncode(path string) specid.ID {
	return specid.ID{
		Type:        specid.Playlist,
		StringValue: base64.URLEncoding.EncodeToString([]byte(path)),
	}
}

func playlistIDDecode(id specid.ID) string {
	path, _ := base64.URLEncoding.DecodeString(id.StringValue)
	return string(path)
}

func playlistRender(c *Controller, params paramsp.Params, playlist *playlistp.Playlist, playlistID specid.ID, withItems bool) (*spec.Playlist, error) {
	user := &db.User{}
	if err := c.dbc.Where("id=?", playlist.UserID).Find(user).Error; err != nil {
		return nil, fmt.Errorf("find user by id: %w", err)
	}

	resp := &spec.Playlist{
		ID:        playlistID,
		Name:      playlist.Name,
		Comment:   playlist.Comment,
		Created:   playlist.UpdatedAt,
		Changed:   playlist.UpdatedAt,
		SongCount: len(playlist.Items),
		Public:    playlist.IsPublic,
		Owner:     user.Name,
	}
	if !withItems {
		return resp, nil
	}

	transcodeMeta := streamGetTranscodeMeta(c.dbc, user.ID, params.GetOr("c", ""))

	for _, path := range playlist.Items {
		id, err := specidpaths.Lookup(c.dbc, MusicPaths(c.musicPaths), c.podcastsPath, path)
		if err != nil {
			log.Printf("error looking up path %q: %s", path, err)
			continue
		}

		var trch *spec.TrackChild
		switch id.Type {
		case specid.Track:
			var track spec.TrackRow
			if err := c.dbc.Scopes(spec.LoadTrackByFolder(user.ID)).Where("id=?", id.Value).Find(&track).Error; err != nil {
				return nil, fmt.Errorf("load track by id: %w", err)
			}
			trch = spec.NewTCTrackByFolder(&track, track.Album)
			resp.Duration += track.Length
		case specid.PodcastEpisode:
			var pe db.PodcastEpisode
			if err := c.dbc.Preload("Podcast").Where("id=?", id.Value).Find(&pe).Error; err != nil {
				return nil, fmt.Errorf("load podcast episode by id: %w", err)
			}
			trch = spec.NewTCPodcastEpisode(&pe)
			resp.Duration += pe.Length
		default:
			continue
		}
		trch.TranscodeMeta = transcodeMeta
		resp.List = append(resp.List, trch)
	}

	resp.SongCount = len(resp.List)

	return resp, nil
}
