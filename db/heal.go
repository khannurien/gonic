package db

import (
	"fmt"
)

// the scanner hard deletes album rows that it didn't see (cleanAlbums), and album ids are
// autoincrement. so renaming a folder, adding an exclude pattern or rebuilding the db all
// give the same album a new id, orphaning anything that pointed at the old one. rows that
// reference an album keep a snapshot of its path triple alongside album_id, and these
// re-resolve that snapshot after a scan.
//
// this has to run at scan time rather than lazily on read: the cover override is consumed
// by the spec constructors through a join on album_id, so a lazy heal would fix the bytes
// getCoverArt serves while every listing still showed no cover art at all.

const healRelinkFmt = `
	UPDATE %s SET album_id = (
		SELECT albums.id FROM albums
		WHERE albums.root_dir=%s.root_dir
		  AND albums.left_path=%s.left_path
		  AND albums.right_path=%s.right_path
	)
	WHERE album_id IS NULL AND EXISTS (
		SELECT 1 FROM albums
		WHERE albums.root_dir=%s.root_dir
		  AND albums.left_path=%s.left_path
		  AND albums.right_path=%s.right_path
	)
`

// orphan rows whose album_id no longer points at the album they snapshotted. this can
// happen if a folder is reused for different content between scans
const healOrphanFmt = `
	UPDATE %s SET album_id = NULL
	WHERE album_id IS NOT NULL AND album_id NOT IN (
		SELECT albums.id FROM albums
		WHERE albums.root_dir=%s.root_dir
		  AND albums.left_path=%s.left_path
		  AND albums.right_path=%s.right_path
	)
`

func (db *DB) healAlbumRefs(table string) (int64, error) {
	orphan := db.Exec(fmt.Sprintf(healOrphanFmt, table, table, table, table))
	if err := orphan.Error; err != nil {
		return 0, fmt.Errorf("orphan stale %s rows: %w", table, err)
	}
	relink := db.Exec(fmt.Sprintf(healRelinkFmt, table, table, table, table, table, table, table))
	if err := relink.Error; err != nil {
		return 0, fmt.Errorf("relink %s rows: %w", table, err)
	}
	return relink.RowsAffected, nil
}

// HealAlbumCoverOverrides re-resolves cover overrides whose album row was deleted and
// recreated by the scanner, then republishes them onto the album rows that the read path
// actually consults. it returns the number of overrides relinked.
func (db *DB) HealAlbumCoverOverrides() (int64, error) {
	n, err := db.healAlbumRefs("album_cover_overrides")
	if err != nil {
		return 0, err
	}
	if err := db.SyncAlbumCoverOverrides(); err != nil {
		return 0, err
	}
	return n, nil
}

// SyncAlbumCoverOverrides republishes album_cover_overrides onto the denormalised
// albums.cover_override_hash/ext columns, and clears them where no override applies.
func (db *DB) SyncAlbumCoverOverrides() error {
	if err := db.Exec(`
		UPDATE albums SET
			cover_override_hash = (SELECT o.hash FROM album_cover_overrides o WHERE o.album_id=albums.id),
			cover_override_ext  = (SELECT o.ext  FROM album_cover_overrides o WHERE o.album_id=albums.id)
		WHERE EXISTS (SELECT 1 FROM album_cover_overrides o WHERE o.album_id=albums.id)
	`).Error; err != nil {
		return fmt.Errorf("publish cover overrides onto albums: %w", err)
	}
	if err := db.Exec(`
		UPDATE albums SET cover_override_hash = '', cover_override_ext = ''
		WHERE coalesce(cover_override_hash, '') <> ''
		  AND NOT EXISTS (SELECT 1 FROM album_cover_overrides o WHERE o.album_id=albums.id)
	`).Error; err != nil {
		return fmt.Errorf("clear stale cover overrides from albums: %w", err)
	}
	return nil
}

// HealCollectionAlbums re-resolves collection entries whose album row was deleted and
// recreated by the scanner. it returns the number of rows relinked.
func (db *DB) HealCollectionAlbums() (int64, error) {
	return db.healAlbumRefs("collection_albums")
}
