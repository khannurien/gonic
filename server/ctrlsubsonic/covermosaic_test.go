package ctrlsubsonic

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"go.senan.xyz/gonic/collection"
	"go.senan.xyz/gonic/db"
)

// setAlbumCovers gives each album a real, decodable cover in the covers store
func setAlbumCovers(t *testing.T, f *fixture, albums ...*db.Album) {
	t.Helper()
	shades := []color.RGBA{{R: 0xff, A: 0xff}, {G: 0xff, A: 0xff}, {B: 0xff, A: 0xff}, {R: 0xff, G: 0xff, A: 0xff}}
	for i, album := range albums {
		cover, err := f.contr.coverStore.Put(newPNGReader(t, 200, 200, shades[i%len(shades)]))
		require.NoError(t, err)
		require.NoError(t, f.contr.setAlbumCoverOverride(album, cover, "upload", "test.png", f.admin))
	}
}

func TestCollectionMosaic(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		albums  int
		wantDim int
	}{
		// one album is passed through untouched rather than made into a 1-up mosaic
		{"one album is passed through", 1, 200},
		{"two albums compose", 2, mosaicSize},
		{"three albums compose", 3, mosaicSize},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)

			all := []*db.Album{&f.albumAA, &f.albumAB, &f.albumBA}
			setAlbumCovers(t, f, all[:tc.albums]...)

			coll := db.Collection{UserID: f.admin.ID, Name: "mosaic"}
			require.NoError(t, f.dbc.Save(&coll).Error)
			ids := make([]int, 0, tc.albums)
			for _, album := range all[:tc.albums] {
				ids = append(ids, album.ID)
			}
			require.NoError(t, collection.SetAlbums(f.dbc, coll.ID, ids))

			body, err := coverForCollection(f.dbc, f.contr.coverStore, f.contr.tagReader, coll.ID)
			require.NoError(t, err)
			t.Cleanup(func() { body.Close() })

			cfg, _, err := image.DecodeConfig(body)
			require.NoError(t, err)
			require.Equal(t, tc.wantDim, cfg.Width)
			require.Equal(t, tc.wantDim, cfg.Height)
		})
	}
}

func TestCollectionMosaicEmpty(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	// the seeded collection's albums have no decodable art, so there's nothing to compose.
	// clients fall back to their own placeholder rather than us inventing one
	_, err := coverForCollection(f.dbc, f.contr.coverStore, f.contr.tagReader, f.collectionShared.ID)
	require.ErrorIs(t, err, errCoverEmpty)
}

func TestCollectionMosaicSkipsAlbumsWithoutArt(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	// only the middle album has art, so this must fall into the single-album path
	setAlbumCovers(t, f, &f.albumAB)

	coll := db.Collection{UserID: f.admin.ID, Name: "sparse"}
	require.NoError(t, f.dbc.Save(&coll).Error)
	require.NoError(t, collection.SetAlbums(f.dbc, coll.ID, []int{f.albumAA.ID, f.albumAB.ID, f.albumBA.ID}))

	body, err := coverForCollection(f.dbc, f.contr.coverStore, f.contr.tagReader, coll.ID)
	require.NoError(t, err)
	t.Cleanup(func() { body.Close() })

	cfg, _, err := image.DecodeConfig(body)
	require.NoError(t, err)
	require.Equal(t, 200, cfg.Width)
}

// pngHeader builds a png that declares w by h but carries no image data. real decompression
// bombs are small files with large headers, and this is the cheap way to make one.
func pngHeader(w, h int) []byte {
	var ihdr bytes.Buffer
	ihdr.WriteString("IHDR")
	_ = binary.Write(&ihdr, binary.BigEndian, uint32(w))
	_ = binary.Write(&ihdr, binary.BigEndian, uint32(h))
	ihdr.Write([]byte{8, 6, 0, 0, 0}) // 8 bit truecolor with alpha, no interlace

	var out bytes.Buffer
	out.Write([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'})
	_ = binary.Write(&out, binary.BigEndian, uint32(ihdr.Len()-4))
	out.Write(ihdr.Bytes())
	_ = binary.Write(&out, binary.BigEndian, crc32.ChecksumIEEE(ihdr.Bytes()))
	return out.Bytes()
}

// a mosaic tile is read straight off disk, so nothing has checked it the way covers.Put
// checks an upload. it has to do its own preflight before Decode allocates.
func TestCollectionMosaicRejectsOversizedTile(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	path := filepath.Join(f.albumAB.RootDir, f.albumAB.LeftPath, f.albumAB.RightPath, f.albumAB.Cover)
	require.NotEmpty(t, f.albumAB.Cover)
	require.NoError(t, os.WriteFile(path, pngHeader(6000, 6000), 0o600))

	_, err := albumCoverImage(f.dbc, f.contr.coverStore, f.contr.tagReader, f.albumAB.ID)
	require.ErrorContains(t, err, "too large", "must be refused on the header, not after decoding")
}
