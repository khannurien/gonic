// Package collection holds the logic behind collections: ordered lists of whole albums.
//
// unlike playlists (m3u files on disk, see the playlist package) collections live in the
// db, because their members change under them when the library is rescanned and reordering
// has to be reliable. what's stored is the album list; the track list is always derived.
package collection

import (
	"errors"
	"fmt"

	"github.com/jinzhu/gorm"

	"go.senan.xyz/gonic/db"
)

var ErrNotFound = errors.New("collection not found")

// Visible scopes to collections a user may read: their own, plus public ones from others.
// this mirrors the playlist rule in ServeGetPlaylists.
func Visible(userID int) func(*gorm.DB) *gorm.DB {
	return func(q *gorm.DB) *gorm.DB {
		return q.Where("collections.user_id=? OR collections.is_public=1", userID)
	}
}

// SetAlbums replaces a collection's whole ordered album list.
//
// it deletes and reinserts rather than shuffling positions: the unique
// (collection_id, position) index makes any incremental reorder hit transient collisions,
// and collections are small. album ids that don't resolve are dropped.
func SetAlbums(dbc *db.DB, collectionID int, albumIDs []int) error {
	return dbc.Transaction(func(tx *db.DB) error {
		if err := tx.Where("collection_id=?", collectionID).Delete(db.CollectionAlbum{}).Error; err != nil {
			return fmt.Errorf("clear collection albums: %w", err)
		}

		var position int
		for _, albumID := range albumIDs {
			var album db.Album
			if err := tx.Select("id, root_dir, left_path, right_path").Where("id=?", albumID).First(&album).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					continue
				}
				return fmt.Errorf("find album %d: %w", albumID, err)
			}
			entry := db.CollectionAlbum{
				CollectionID: collectionID,
				Position:     position,
				AlbumID:      &album.ID,
				// snapshot so the entry survives the album row being deleted and
				// recreated by a rescan, see db.HealCollectionAlbums
				RootDir:   album.RootDir,
				LeftPath:  album.LeftPath,
				RightPath: album.RightPath,
			}
			if err := tx.Save(&entry).Error; err != nil {
				return fmt.Errorf("save collection album: %w", err)
			}
			position++
		}
		return nil
	})
}

// AlbumIDs returns the collection's albums in order. entries whose album row is gone (and
// which healing couldn't recover) are skipped rather than surfacing as broken rows.
func AlbumIDs(dbc *db.DB, collectionID int) ([]int, error) {
	var entries []db.CollectionAlbum
	err := dbc.
		Where("collection_id=? AND album_id IS NOT NULL", collectionID).
		Order("position").
		Find(&entries).Error
	if err != nil {
		return nil, fmt.Errorf("find collection albums: %w", err)
	}
	ids := make([]int, 0, len(entries))
	for _, entry := range entries {
		ids = append(ids, *entry.AlbumID)
	}
	return ids, nil
}

// Stats returns the collection's derived track count and total duration. both are derived
// rather than stored, so they can't drift from the library.
func Stats(dbc *db.DB, collectionID int) (trackCount int, duration int, err error) {
	row := dbc.
		Model(db.Track{}).
		Select("count(tracks.id), coalesce(sum(tracks.length), 0)").
		Joins("JOIN collection_albums ca ON ca.album_id=tracks.album_id").
		Where("ca.collection_id=?", collectionID).
		Row()
	if err := row.Scan(&trackCount, &duration); err != nil {
		return 0, 0, fmt.Errorf("scan collection stats: %w", err)
	}
	return trackCount, duration, nil
}
