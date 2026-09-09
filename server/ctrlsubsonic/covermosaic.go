package ctrlsubsonic

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"os"
	"path/filepath"

	"github.com/disintegration/imaging"

	"go.senan.xyz/gonic/collection"
	"go.senan.xyz/gonic/covers"
	"go.senan.xyz/gonic/db"
	"go.senan.xyz/gonic/tags"
)

const (
	mosaicSize        = 1000
	mosaicMaxTiles    = 4
	mosaicJPEGQuality = 90
)

// coverForCollection builds a collection's cover from its albums' art.
//
// the layouts are spotify shaped rather than repeating a cover to fill the grid, which
// reads as broken. a single album is passed through untouched: cheaper, and better looking
// than a one up mosaic.
func coverForCollection(dbc *db.DB, coverStore *covers.Store, tagReader tags.Reader, collectionID int) (io.ReadCloser, error) {
	albumIDs, err := collection.AlbumIDs(dbc, collectionID)
	if err != nil {
		return nil, fmt.Errorf("find collection albums: %w", err)
	}

	images := make([]image.Image, 0, mosaicMaxTiles)
	for _, albumID := range albumIDs {
		img, err := albumCoverImage(dbc, coverStore, tagReader, albumID)
		if err != nil {
			continue // an album with no art, or art we can't decode, just doesn't count
		}
		images = append(images, img)
		if len(images) == mosaicMaxTiles {
			break
		}
	}

	switch len(images) {
	case 0:
		return nil, errCoverEmpty
	case 1:
		return encodeJPEG(images[0])
	}
	return encodeJPEG(composeMosaic(images))
}

// albumCoverImage resolves one album's art the same way coverForAlbum does, but also falls
// back to a track's embedded cover. the mosaic is ours rather than spec mandated, so it may
// as well work for embedded-only libraries.
func albumCoverImage(dbc *db.DB, coverStore *covers.Store, tagReader tags.Reader, albumID int) (image.Image, error) {
	var album db.Album
	err := dbc.
		Select("id, root_dir, left_path, right_path, cover, cover_override_hash, cover_override_ext, embedded_cover_track_id").
		First(&album, albumID).Error
	if err != nil {
		return nil, fmt.Errorf("select album: %w", err)
	}

	var body io.ReadCloser
	switch {
	case album.CoverOverrideHash != "" && coverStore != nil:
		body, err = coverStore.Open(album.CoverOverrideHash, album.CoverOverrideExt)
	case album.Cover != "":
		body, err = os.Open(filepath.Join(album.RootDir, album.LeftPath, album.RightPath, album.Cover))
	case album.EmbeddedCoverTrackID != nil:
		body, err = coverForTrack(dbc, tagReader, *album.EmbeddedCoverTrackID)
	default:
		return nil, errCoverEmpty
	}
	if err != nil {
		return nil, fmt.Errorf("open album cover: %w", err)
	}
	defer body.Close()

	img, _, err := image.Decode(body)
	if err != nil {
		return nil, fmt.Errorf("decode album cover: %w", err)
	}
	return img, nil
}

// composeMosaic lays two, three or four covers out on a square canvas.
//
//	2: two half width panels
//	3: one full height left half, two stacked quarters on the right
//	4: an even 2x2
func composeMosaic(images []image.Image) image.Image {
	canvas := imaging.New(mosaicSize, mosaicSize, image.Black)
	half := mosaicSize / 2

	type tile struct{ x, y, w, h int }
	var tiles []tile
	switch len(images) {
	case 2:
		tiles = []tile{{0, 0, half, mosaicSize}, {half, 0, half, mosaicSize}}
	case 3:
		tiles = []tile{{0, 0, half, mosaicSize}, {half, 0, half, half}, {half, half, half, half}}
	default:
		tiles = []tile{{0, 0, half, half}, {half, 0, half, half}, {0, half, half, half}, {half, half, half, half}}
	}

	for i, t := range tiles {
		if i >= len(images) {
			break
		}
		// Fill, not Fit: Fill crops to the tile's aspect, Fit would letterbox with gutters
		filled := imaging.Fill(images[i], t.w, t.h, imaging.Center, imaging.Lanczos)
		canvas = imaging.Paste(canvas, filled, image.Pt(t.x, t.y))
	}
	return canvas
}

func encodeJPEG(img image.Image) (io.ReadCloser, error) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: mosaicJPEGQuality}); err != nil {
		return nil, fmt.Errorf("encode mosaic: %w", err)
	}
	return io.NopCloser(&buf), nil
}
