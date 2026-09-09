package covers

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func opaquePNG(tb testing.TB, w, h int) []byte {
	tb.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 0x40, A: 0xff})
		}
	}
	var buf bytes.Buffer
	require.NoError(tb, png.Encode(&buf, img))
	return buf.Bytes()
}

func opaqueJPEG(tb testing.TB, w, h int) []byte {
	tb.Helper()
	img := image.NewYCbCr(image.Rect(0, 0, w, h), image.YCbCrSubsampleRatio420)
	var buf bytes.Buffer
	require.NoError(tb, jpeg.Encode(&buf, img, nil))
	return buf.Bytes()
}

func newStore(tb testing.TB) *Store {
	tb.Helper()
	store, err := NewStore(tb.TempDir())
	require.NoError(tb, err)
	return store
}

func TestPutOpenDelete(t *testing.T) {
	t.Parallel()
	store := newStore(t)

	cover, err := store.Put(bytes.NewReader(opaqueJPEG(t, 300, 300)))
	require.NoError(t, err)
	require.Len(t, cover.Hash, 64)
	require.Equal(t, ExtJPG, cover.Ext)
	require.Equal(t, "image/jpeg", cover.MIME)
	require.Equal(t, 300, cover.Width)
	require.Equal(t, 300, cover.Height)
	require.Positive(t, cover.Size)

	path, err := store.Path(cover.Hash, cover.Ext)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(store.BasePath(), cover.Hash[:2], cover.Hash+".jpg"), path)

	f, err := store.Open(cover.Hash, cover.Ext)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	require.NoError(t, store.Delete(cover.Hash, cover.Ext))
	_, err = store.Open(cover.Hash, cover.Ext)
	require.True(t, os.IsNotExist(err))

	// delete is idempotent
	require.NoError(t, store.Delete(cover.Hash, cover.Ext))
}

func TestPutDedupes(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	raw := opaquePNG(t, 100, 100)

	a, err := store.Put(bytes.NewReader(raw))
	require.NoError(t, err)
	b, err := store.Put(bytes.NewReader(raw))
	require.NoError(t, err)
	require.Equal(t, a.Hash, b.Hash)

	var count int
	require.NoError(t, store.Walk(func(string, string, os.FileInfo) error { count++; return nil }))
	require.Equal(t, 1, count)
}

func TestPutTranscodesAlphaToPNGAndOpaqueToJPEG(t *testing.T) {
	t.Parallel()
	store := newStore(t)

	alpha := image.NewNRGBA(image.Rect(0, 0, 10, 10))
	alpha.Set(0, 0, color.NRGBA{R: 0xff, A: 0x80})
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, alpha))

	withAlpha, err := store.Put(bytes.NewReader(buf.Bytes()))
	require.NoError(t, err)
	require.Equal(t, ExtPNG, withAlpha.Ext)

	opaque, err := store.Put(bytes.NewReader(opaqueJPEG(t, 10, 10)))
	require.NoError(t, err)
	require.Equal(t, ExtJPG, opaque.Ext)
}

func TestPutDownscalesOversizedImages(t *testing.T) {
	t.Parallel()
	store := newStore(t)

	cover, err := store.Put(bytes.NewReader(opaqueJPEG(t, StoreDimension+500, StoreDimension+250)))
	require.NoError(t, err)
	require.Equal(t, StoreDimension, cover.Width)
	require.LessOrEqual(t, cover.Height, StoreDimension)
	// the resize hands back an *image.NRGBA whatever went in, so an opaque photo must
	// still come out as a jpeg rather than a lossless png
	require.Equal(t, ExtJPG, cover.Ext)
}

func TestPutRejectsNonImages(t *testing.T) {
	t.Parallel()
	store := newStore(t)

	_, err := store.Put(strings.NewReader("this is definitely not an image"))
	require.ErrorIs(t, err, ErrNotAnImage)
}

func TestPutRejectsOversizedBodies(t *testing.T) {
	t.Parallel()
	store := newStore(t)

	_, err := store.Put(bytes.NewReader(make([]byte, MaxBytes+1)))
	require.ErrorIs(t, err, ErrTooLarge)
}

func TestPathRejectsTraversalAndBadExt(t *testing.T) {
	t.Parallel()
	store := newStore(t)

	for _, hash := range []string{
		"../../etc/passwd",
		"..",
		"",
		strings.Repeat("z", 64), // right length, not hex
		strings.Repeat("a", 63), // hex, wrong length
	} {
		_, err := store.Path(hash, ExtJPG)
		require.ErrorIs(t, err, ErrInvalidHash, "hash %q should be rejected", hash)
	}

	_, err := store.Path(strings.Repeat("a", 64), "exe")
	require.ErrorIs(t, err, ErrInvalidExt)
}

func TestNewStoreRejectsEmptyPath(t *testing.T) {
	t.Parallel()
	_, err := NewStore("")
	require.ErrorIs(t, err, ErrInvalidStore)
}
