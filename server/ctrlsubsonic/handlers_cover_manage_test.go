package ctrlsubsonic

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"go.senan.xyz/gonic/db"
	"go.senan.xyz/gonic/server/ctrlsubsonic/params"
	"go.senan.xyz/gonic/server/ctrlsubsonic/spec"
)

func newPNGReader(tb testing.TB, w, h int, c color.Color) *bytes.Reader {
	tb.Helper()
	return bytes.NewReader(testPNG(tb, w, h, c))
}

func testPNG(tb testing.TB, w, h int, c color.Color) []byte {
	tb.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	require.NoError(tb, png.Encode(&buf, img))
	return buf.Bytes()
}

// uploadCover posts a multipart body the way a client has to: every scalar in the query
// string, only the file in the body.
func uploadCover(tb testing.TB, f *fixture, user *db.User, albumID string, data []byte) *spec.Response {
	tb.Helper()

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("file", "cover.png")
	require.NoError(tb, err)
	_, err = part.Write(data)
	require.NoError(tb, err)
	require.NoError(tb, mw.Close())

	// every scalar goes in the query string: params.New snapshots r.Form before any
	// multipart parse, so multipart fields would be invisible to the params api
	query := url.Values{"id": {albumID}, "f": {"json"}, "u": {user.Name}, "p": {user.Password}, "v": {"1"}, "c": {mockClientName}}
	req := httptest.NewRequest(http.MethodPost, "/uploadCoverArt?"+query.Encode(), &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	ctx := context.WithValue(req.Context(), CtxParams, params.New(req))
	ctx = context.WithValue(ctx, CtxUser, user)
	req = req.WithContext(ctx)

	rr := httptest.NewRecorder()
	resp := f.contr.ServeUploadCoverArt(rr, req)
	require.NotNil(tb, resp, "upload handler must return a spec response")
	return resp
}

func decodeCoverArt(t *testing.T, f *fixture, user *db.User, albumID string) *spec.CoverArt {
	t.Helper()
	raw := f.query(t, f.contr.ServeGetCoverArtInfo, user, url.Values{"id": {albumID}})
	var got struct {
		Response struct {
			CoverArt *spec.CoverArt `json:"coverArt"`
		} `json:"subsonic-response"`
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &got))
	return got.Response.CoverArt
}

func TestCoverArtUploadOverridesAndClears(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	albumID := f.albumAA.SID().String()

	before := decodeCoverArt(t, f, f.admin, albumID)
	require.False(t, before.Overridden)

	resp := uploadCover(t, f, f.admin, albumID, testPNG(t, 64, 64, color.RGBA{R: 0xff, A: 0xff}))
	require.Nil(t, resp.Error)
	require.NotNil(t, resp.CoverArt)
	require.True(t, resp.CoverArt.Overridden)
	require.Equal(t, "upload", resp.CoverArt.Source)
	require.Equal(t, 64, resp.CoverArt.Width)
	require.NotEmpty(t, resp.CoverArt.Token)
	// an overridden album serves its art under the album id, not an embedded track id
	require.Equal(t, albumID, resp.CoverArt.CoverID.String())

	after := decodeCoverArt(t, f, f.admin, albumID)
	require.True(t, after.Overridden)
	require.NotEqual(t, before.Token, after.Token, "token must change so clients refetch")

	// the bytes are served back through the normal cover path
	body, err := coverForAlbum(f.dbc, f.contr.coverStore, f.albumAA.ID)
	require.NoError(t, err)
	t.Cleanup(func() { body.Close() })
	cfg, format, err := image.DecodeConfig(body)
	require.NoError(t, err)
	require.Equal(t, "png", format)
	require.Equal(t, 64, cfg.Width)

	cleared := f.query(t, f.contr.ServeDeleteCoverArt, f.admin, url.Values{"id": {albumID}})
	require.NotContains(t, cleared, `"error"`)

	final := decodeCoverArt(t, f, f.admin, albumID)
	require.False(t, final.Overridden)
	require.Empty(t, final.Source)

	var count int
	require.NoError(t, f.dbc.Model(db.AlbumCoverOverride{}).Count(&count).Error)
	require.Zero(t, count, "clearing must remove the override row")
}

func TestCoverArtRequiresAdmin(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	albumID := f.albumAA.SID().String()

	for name, run := range map[string]func() string{
		"info":   func() string { return f.query(t, f.contr.ServeGetCoverArtInfo, f.alt, url.Values{"id": {albumID}}) },
		"delete": func() string { return f.query(t, f.contr.ServeDeleteCoverArt, f.alt, url.Values{"id": {albumID}}) },
		"set": func() string {
			return f.query(t, f.contr.ServeSetCoverArt, f.alt, url.Values{"id": {albumID}, "url": {"http://example.com/a.png"}})
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			require.Contains(t, run(), "user not admin")
		})
	}

	resp := uploadCover(t, f, f.alt, albumID, testPNG(t, 8, 8, color.Black))
	require.NotNil(t, resp.Error)
	require.Contains(t, resp.Error.Message, "user not admin")
}

func TestCoverArtSetFromURL(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	data := testPNG(t, 120, 120, color.RGBA{B: 0xff, A: 0xff})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Contains(t, r.Header.Get("User-Agent"), "gonic")
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(data)
	}))
	t.Cleanup(server.Close)
	// the real client refuses loopback, see refusePrivateAddr
	f.contr.coverFetchClient = server.Client()

	albumID := f.albumAB.SID().String()
	out := f.query(t, f.contr.ServeSetCoverArt, f.admin, url.Values{"id": {albumID}, "url": {server.URL + "/cover.png"}})
	require.NotContains(t, out, `"error"`)

	got := decodeCoverArt(t, f, f.admin, albumID)
	require.True(t, got.Overridden)
	require.Equal(t, "url", got.Source)
	require.Equal(t, 120, got.Width)
}

func TestCoverArtSetFromURLRejectsBadScheme(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	out := f.query(t, f.contr.ServeSetCoverArt, f.admin,
		url.Values{"id": {f.albumAA.SID().String()}, "url": {"file:///etc/passwd"}})
	require.Contains(t, out, "unsupported url scheme")
}

func TestCoverArtUploadRejectsNonImage(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	resp := uploadCover(t, f, f.admin, f.albumAA.SID().String(), []byte("not an image at all"))
	require.NotNil(t, resp.Error)
	require.Contains(t, strings.ToLower(resp.Error.Message), "not a decodable image")
}

func TestCoverArtSetFromURLRefusesPrivateAddresses(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	// no injected client here, so the guarded default applies
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(testPNG(t, 8, 8, color.White))
	}))
	t.Cleanup(server.Close)

	out := f.query(t, f.contr.ServeSetCoverArt, f.admin,
		url.Values{"id": {f.albumAA.SID().String()}, "url": {server.URL + "/cover.png"}})
	require.Contains(t, out, "non public address")
}
