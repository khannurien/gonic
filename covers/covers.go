// Package covers is a content addressed store for cover art that an admin set from a
// client. it lives in gonic's own directory, never in the music tree, and never under the
// cover cache dir (which is LRU ejected).
package covers

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/disintegration/imaging"

	// image.Decode needs these registered. jpeg/png/gif come from the stdlib, bmp/tiff
	// come in transitively with imaging, webp is ours: half the cover urls a user pastes
	// are webp, and there is no webp encoder in go so Put transcodes them
	_ "golang.org/x/image/webp"

	"go.senan.xyz/gonic/fileutil"
)

const (
	// MaxBytes is the hard cap on any incoming image
	MaxBytes = 15 << 20
	// MaxDimension and MaxPixels guard against decompression bombs. they're checked
	// against DecodeConfig, before Decode allocates anything
	MaxDimension = 10000
	MaxPixels    = 50_000_000
	// StoreDimension is what everything is downscaled to fit
	StoreDimension = 1500

	ExtJPG = "jpg"
	ExtPNG = "png"

	jpegQuality = 90
)

var (
	ErrTooLarge     = errors.New("image too large")
	ErrNotAnImage   = errors.New("not a decodable image")
	ErrInvalidHash  = errors.New("invalid hash")
	ErrInvalidExt   = errors.New("invalid ext")
	ErrInvalidStore = errors.New("invalid store path")
)

//nolint:gochecknoglobals
var hashExpr = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Cover is the metadata of a stored image. Hash is the sha256 of the normalized bytes, so
// the same picture arriving from two different sources is stored once.
type Cover struct {
	Hash          string
	Ext           string
	MIME          string
	Width, Height int
	Size          int
}

type Store struct {
	basePath string
}

func NewStore(basePath string) (*Store, error) {
	if basePath == "" {
		return nil, ErrInvalidStore
	}
	if err := os.MkdirAll(filepath.Join(basePath, "tmp"), 0o755); err != nil {
		return nil, fmt.Errorf("make covers dir: %w", err)
	}
	return &Store{basePath: basePath}, nil
}

func (s *Store) BasePath() string { return s.basePath }

// Path returns the absolute path of a stored cover. hash and ext are validated before
// joining because they can come from a db row.
func (s *Store) Path(hash, ext string) (string, error) {
	if !hashExpr.MatchString(hash) {
		return "", fmt.Errorf("%w: %q", ErrInvalidHash, hash)
	}
	if ext != ExtJPG && ext != ExtPNG {
		return "", fmt.Errorf("%w: %q", ErrInvalidExt, ext)
	}
	return fileutil.SafeJoin(s.basePath, filepath.Join(hash[:2], hash+"."+ext))
}

func (s *Store) Open(hash, ext string) (*os.File, error) {
	path, err := s.Path(hash, ext)
	if err != nil {
		return nil, err
	}
	return os.Open(path)
}

// Delete removes a stored cover. it does no reference counting: two albums can legitimately
// share a hash, so the caller must check that no row still references it.
func (s *Store) Delete(hash, ext string) error {
	path, err := s.Path(hash, ext)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove cover: %w", err)
	}
	return nil
}

// Put decodes, validates, normalizes and stores an image, returning its metadata.
//
// it always re-encodes rather than storing the original bytes. that costs a little quality
// but means attacker controlled containers (exif, icc, trailing payloads, polyglots,
// animated gifs) never touch disk, the stored dimensions and mime always match the file,
// and content addressing is canonical so duplicates collapse.
func (s *Store) Put(r io.Reader) (Cover, error) {
	var raw bytes.Buffer
	if _, err := io.Copy(&raw, io.LimitReader(r, MaxBytes+1)); err != nil {
		return Cover{}, fmt.Errorf("read image: %w", err)
	}
	if raw.Len() > MaxBytes {
		return Cover{}, fmt.Errorf("%w: over %d bytes", ErrTooLarge, MaxBytes)
	}

	conf, _, err := image.DecodeConfig(bytes.NewReader(raw.Bytes()))
	if err != nil {
		return Cover{}, fmt.Errorf("%w: %v", ErrNotAnImage, err) //nolint:errorlint // wrapping our own sentinel
	}
	if conf.Width > MaxDimension || conf.Height > MaxDimension || conf.Width*conf.Height > MaxPixels {
		return Cover{}, fmt.Errorf("%w: %dx%d", ErrTooLarge, conf.Width, conf.Height)
	}

	img, _, err := image.Decode(bytes.NewReader(raw.Bytes()))
	if err != nil {
		return Cover{}, fmt.Errorf("%w: %v", ErrNotAnImage, err) //nolint:errorlint // wrapping our own sentinel
	}
	if b := img.Bounds(); b.Dx() > StoreDimension || b.Dy() > StoreDimension {
		img = imaging.Fit(img, StoreDimension, StoreDimension, imaging.Lanczos)
	}

	var enc bytes.Buffer
	cover := Cover{
		Width:  img.Bounds().Dx(),
		Height: img.Bounds().Dy(),
	}
	if hasAlpha(img) {
		cover.Ext, cover.MIME = ExtPNG, "image/png"
		err = png.Encode(&enc, img)
	} else {
		cover.Ext, cover.MIME = ExtJPG, "image/jpeg"
		err = jpeg.Encode(&enc, img, &jpeg.Options{Quality: jpegQuality})
	}
	if err != nil {
		return Cover{}, fmt.Errorf("encode image: %w", err)
	}

	sum := sha256.Sum256(enc.Bytes())
	cover.Hash = hex.EncodeToString(sum[:])
	cover.Size = enc.Len()

	if err := s.write(cover, enc.Bytes()); err != nil {
		return Cover{}, err
	}
	return cover, nil
}

func (s *Store) write(cover Cover, data []byte) error {
	path, err := s.Path(cover.Hash, cover.Ext)
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil {
		return nil // already stored, dedupe is free
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("make cover shard dir: %w", err)
	}
	// write then rename so a reader never sees a half written cover. same filesystem, so
	// the rename is atomic
	tmp, err := os.CreateTemp(filepath.Join(s.basePath, "tmp"), "put-*")
	if err != nil {
		return fmt.Errorf("create temp cover: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp cover: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp cover: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("rename cover into place: %w", err)
	}
	return nil
}

// Walk calls fn for every stored cover. used to garbage collect blobs no row references.
func (s *Store) Walk(fn func(hash, ext string, info os.FileInfo) error) error {
	shards, err := os.ReadDir(s.basePath)
	if err != nil {
		return fmt.Errorf("read covers dir: %w", err)
	}
	for _, shard := range shards {
		if !shard.IsDir() || shard.Name() == "tmp" {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(s.basePath, shard.Name()))
		if err != nil {
			return fmt.Errorf("read covers shard: %w", err)
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			hash, ext, ok := strings.Cut(entry.Name(), ".")
			if !ok {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				return fmt.Errorf("stat cover: %w", err)
			}
			if err := fn(hash, ext, info); err != nil {
				return err
			}
		}
	}
	return nil
}

func hasAlpha(img image.Image) bool {
	switch img.ColorModel() {
	case color.NRGBAModel, color.RGBAModel, color.NRGBA64Model, color.RGBA64Model, color.AlphaModel, color.Alpha16Model:
		return true
	}
	return false
}
