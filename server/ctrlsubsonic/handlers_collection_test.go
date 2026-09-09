package ctrlsubsonic

import (
	"encoding/json"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"

	"go.senan.xyz/gonic/collection"
	"go.senan.xyz/gonic/db"
	"go.senan.xyz/gonic/server/ctrlsubsonic/spec"
)

func TestGetCollections(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.run(t, f.contr.ServeGetCollections, f.admin,
		query{url.Values{}, "admin", false},
	)
	f.run(t, f.contr.ServeGetCollections, f.alt,
		query{url.Values{}, "alt_sees_own_and_public", false},
	)
}

func TestGetCollection(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.run(t, f.contr.ServeGetCollection, f.admin,
		query{url.Values{"id": {f.collectionShared.SID().String()}}, "shared", false},
	)
}

func TestGetCollectionNotVisible(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	out := f.query(t, f.contr.ServeGetCollection, f.admin,
		url.Values{"id": {f.collectionPrivate.SID().String()}})
	require.Contains(t, out, "aren't allowed to read")
}

// the collection's own album order must win over anything the database returns
func TestGetCollectionKeepsAlbumOrder(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	out := f.query(t, f.contr.ServeGetCollection, f.admin,
		url.Values{"id": {f.collectionShared.SID().String()}})

	var got struct {
		Response struct {
			Collection *spec.Collection `json:"collection"`
		} `json:"subsonic-response"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got))

	coll := got.Response.Collection
	require.NotNil(t, coll)
	require.Len(t, coll.List, 2)
	require.Equal(t, f.albumAB.SID().String(), coll.List[0].ID.String())
	require.Equal(t, f.albumAA.SID().String(), coll.List[1].ID.String())

	// tracks are expanded album by album, in collection order
	require.NotEmpty(t, coll.Tracks)
	require.Equal(t, f.albumAB.SID().String(), coll.Tracks[0].AlbumID.String())
	require.Equal(t, f.albumAA.SID().String(), coll.Tracks[len(coll.Tracks)-1].AlbumID.String())
	require.Equal(t, coll.SongCount, len(coll.Tracks), "song count must match what's returned")
}

func TestCreateUpdateDeleteCollection(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	out := f.query(t, f.contr.ServeCreateCollection, f.admin, url.Values{
		"name":    {"new collection"},
		"comment": {"made in a test"},
		"public":  {"true"},
		"albumId": {f.albumAA.SID().String(), f.albumBA.SID().String()},
	})
	var created struct {
		Response struct {
			Collection *spec.Collection `json:"collection"`
		} `json:"subsonic-response"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &created))
	coll := created.Response.Collection
	require.NotNil(t, coll)
	require.Equal(t, "new collection", coll.Name)
	require.Equal(t, 2, coll.AlbumCount)

	id := coll.ID.String()

	// a full ordered replace covers add, remove and reorder in one call
	out = f.query(t, f.contr.ServeUpdateCollection, f.admin, url.Values{
		"id":      {id},
		"name":    {"renamed"},
		"albumId": {f.albumBA.SID().String(), f.albumAA.SID().String(), f.albumAB.SID().String()},
	})
	require.NotContains(t, out, `"error"`)

	albumIDs, err := collection.AlbumIDs(f.dbc, coll.ID.Value)
	require.NoError(t, err)
	require.Equal(t, []int{f.albumBA.ID, f.albumAA.ID, f.albumAB.ID}, albumIDs)

	var reloaded db.Collection
	require.NoError(t, f.dbc.Where("id=?", coll.ID.Value).First(&reloaded).Error)
	require.Equal(t, "renamed", reloaded.Name)
	require.Equal(t, "made in a test", reloaded.Comment, "an absent param must not clear a field")

	out = f.query(t, f.contr.ServeDeleteCollection, f.admin, url.Values{"id": {id}})
	require.NotContains(t, out, `"error"`)

	var count int
	require.NoError(t, f.dbc.Model(db.Collection{}).Where("id=?", coll.ID.Value).Count(&count).Error)
	require.Zero(t, count)

	// cascade must take the album rows with it
	require.NoError(t, f.dbc.Model(db.CollectionAlbum{}).Where("collection_id=?", coll.ID.Value).Count(&count).Error)
	require.Zero(t, count)
}

func TestUpdateCollectionNotOwner(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	// alt can read the shared collection, but must not be able to change it
	out := f.query(t, f.contr.ServeUpdateCollection, f.alt,
		url.Values{"id": {f.collectionShared.SID().String()}, "name": {"hijacked"}})
	require.Contains(t, out, "aren't allowed to change")

	out = f.query(t, f.contr.ServeDeleteCollection, f.alt,
		url.Values{"id": {f.collectionShared.SID().String()}})
	require.Contains(t, out, "aren't allowed to change")
}

func TestCollectionMirroredAsPlaylist(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	// a vanilla client asks getPlaylist with the id it saw in getPlaylists
	out := f.query(t, f.contr.ServeGetPlaylist, f.admin,
		url.Values{"id": {f.collectionShared.SID().String()}})

	var got struct {
		Response struct {
			Playlist *spec.Playlist `json:"playlist"`
		} `json:"subsonic-response"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got))

	pl := got.Response.Playlist
	require.NotNil(t, pl)
	require.Equal(t, "shared collection", pl.Name)
	require.NotEmpty(t, pl.List, "the mirror must expand to real tracks")
	require.Equal(t, len(pl.List), pl.SongCount)
	// album order is preserved through the mirror too
	require.Equal(t, f.albumAB.SID().String(), pl.List[0].AlbumID.String())
}

func TestCollectionRejectsPlaylistEdits(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	id := f.collectionShared.SID().String()

	for name, out := range map[string]string{
		"update": f.query(t, f.contr.ServeUpdatePlaylist, f.admin,
			url.Values{"id": {id}, "songIndexToRemove": {"0"}}),
		"create": f.query(t, f.contr.ServeCreateOrUpdatePlaylist, f.admin,
			url.Values{"id": {id}, "songId": {"tr-1"}}),
		"delete": f.query(t, f.contr.ServeDeletePlaylist, f.admin,
			url.Values{"id": {id}}),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			require.Contains(t, out, "this playlist is a collection")
		})
	}

	// and nothing was actually changed
	albumIDs, err := collection.AlbumIDs(f.dbc, f.collectionShared.ID)
	require.NoError(t, err)
	require.Len(t, albumIDs, 2)
}

// a rescan that deletes and recreates an album row must not silently empty a collection
func TestCollectionSurvivesAlbumRowRecreation(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	before, err := collection.AlbumIDs(f.dbc, f.collectionShared.ID)
	require.NoError(t, err)
	require.Len(t, before, 2)

	// simulate cleanAlbums dropping the row, which nulls album_id via ON DELETE SET NULL
	require.NoError(t, f.dbc.Where("id=?", f.albumAB.ID).Delete(db.Album{}).Error)

	orphaned, err := collection.AlbumIDs(f.dbc, f.collectionShared.ID)
	require.NoError(t, err)
	require.Len(t, orphaned, 1, "the orphaned entry drops out until it's healed")

	// the album comes back with a *different* id, as a real rescan would give it
	recreated := db.Album{
		RootDir:   f.albumAB.RootDir,
		LeftPath:  f.albumAB.LeftPath,
		RightPath: f.albumAB.RightPath,
		TagTitle:  f.albumAB.TagTitle,
	}
	require.NoError(t, f.dbc.Save(&recreated).Error)
	require.NotEqual(t, f.albumAB.ID, recreated.ID)

	_, err = f.dbc.HealCollectionAlbums()
	require.NoError(t, err)

	after, err := collection.AlbumIDs(f.dbc, f.collectionShared.ID)
	require.NoError(t, err)
	require.Equal(t, []int{recreated.ID, f.albumAA.ID}, after, "healed, and still in order")
}

// an empty albumId is how a client says "no albums". without it a collection could be
// reordered and added to but never emptied.
func TestUpdateCollectionEmptiesAlbums(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	id := f.collectionShared.SID().String()
	out := f.query(t, f.contr.ServeUpdateCollection, f.admin,
		url.Values{"id": {id}, "albumId": {""}})
	require.NotContains(t, out, `"error"`)

	albumIDs, err := collection.AlbumIDs(f.dbc, f.collectionShared.ID)
	require.NoError(t, err)
	require.Empty(t, albumIDs)

	// an absent albumId still leaves the list alone
	require.NoError(t, collection.SetAlbums(f.dbc, f.collectionShared.ID, []int{f.albumAA.ID}))
	out = f.query(t, f.contr.ServeUpdateCollection, f.admin,
		url.Values{"id": {id}, "name": {"renamed"}})
	require.NotContains(t, out, `"error"`)

	albumIDs, err = collection.AlbumIDs(f.dbc, f.collectionShared.ID)
	require.NoError(t, err)
	require.Equal(t, []int{f.albumAA.ID}, albumIDs)
}

func TestCreateCollectionRejectsBadAlbumIDs(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	var before int
	require.NoError(t, f.dbc.Model(db.Collection{}).Count(&before).Error)

	out := f.query(t, f.contr.ServeCreateCollection, f.admin,
		url.Values{"name": {"doomed"}, "albumId": {"tr-1"}})
	require.Contains(t, out, "please provide valid album ids")

	// and the failed call left nothing behind
	var after int
	require.NoError(t, f.dbc.Model(db.Collection{}).Count(&after).Error)
	require.Equal(t, before, after)
}

// getPlaylist takes either `id` or `playlistId`, and the collection mirror has to honour
// both the same way the m3u path does
func TestCollectionMirrorAcceptsPlaylistIDParam(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	out := f.query(t, f.contr.ServeGetPlaylist, f.admin,
		url.Values{"playlistId": {f.collectionShared.SID().String()}})
	require.NotContains(t, out, `"error"`)

	var got struct {
		Response struct {
			Playlist *spec.Playlist `json:"playlist"`
		} `json:"subsonic-response"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	require.NotNil(t, got.Response.Playlist)
	require.Equal(t, "shared collection", got.Response.Playlist.Name)
}
