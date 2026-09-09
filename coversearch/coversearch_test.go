package coversearch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/time/rate"

	"go.senan.xyz/gonic/coverartarchive"
	"go.senan.xyz/gonic/deezer"
	"go.senan.xyz/gonic/itunes"
	"go.senan.xyz/gonic/musicbrainz"
)

const (
	caaReleaseJSON = `{"images":[
		{"image":"https://ia.example/front-1200.jpg","front":true,"types":["Front"],
		 "thumbnails":{"250":"https://ia.example/front-250.jpg","500":"https://ia.example/front-500.jpg"}},
		{"image":"https://ia.example/back.jpg","front":false,"types":["Back"],"thumbnails":{}}
	]}`

	deezerSearchJSON = `{"data":[
		{"title":"OK Computer","artist":{"name":"Radiohead"},
		 "cover_medium":"https://dz.example/250.jpg","cover_big":"https://dz.example/500.jpg","cover_xl":"https://dz.example/1000.jpg"}
	]}`

	itunesSearchJSON = `{"results":[
		{"collectionName":"OK Computer","artistName":"Radiohead",
		 "artworkUrl100":"https://is1.mzstatic.com/image/thumb/abc/100x100bb.jpg"}
	]}`
)

func stubServer(tb testing.TB, body string) *httptest.Server {
	tb.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Contains(tb, r.Header.Get("User-Agent"), "gonic-test")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	tb.Cleanup(server.Close)
	return server
}

// noLimit avoids the real per-second rate limiters in tests
func noLimit() *rate.Limiter { return rate.NewLimiter(rate.Inf, 1) }

func newSearcher(tb testing.TB) *Searcher {
	tb.Helper()

	caa := coverartarchive.NewClient("gonic-test")
	caa.BaseURL = stubServer(tb, caaReleaseJSON).URL + "/"
	caa.Limiter = noLimit()

	dz := deezer.NewClient("gonic-test")
	dz.BaseURL = stubServer(tb, deezerSearchJSON).URL + "/"
	dz.Limiter = noLimit()

	it := itunes.NewClient("gonic-test")
	it.BaseURL = stubServer(tb, itunesSearchJSON).URL + "/"
	it.Limiter = noLimit()

	return &Searcher{CoverArtArchive: caa, Deezer: dz, ITunes: it}
}

func TestSearchMergesSourcesInFixedOrder(t *testing.T) {
	t.Parallel()
	s := newSearcher(t)

	results, err := s.Search(context.Background(), Query{
		Artist: "Radiohead", Album: "OK Computer", MusicBrainzID: "d6591c8e-0000-0000-0000-000000000000",
	})
	require.NoError(t, err)
	require.Len(t, results, 3)

	// fixed source order keeps output deterministic despite the concurrent fan out
	require.Equal(t, SourceCoverArtArchive, results[0].Source)
	require.Equal(t, SourceDeezer, results[1].Source)
	require.Equal(t, SourceITunes, results[2].Source)

	// only front covers are useful as album art
	require.Equal(t, "https://ia.example/front-1200.jpg", results[0].URL)
	require.Equal(t, "https://ia.example/front-250.jpg", results[0].ThumbnailURL)

	require.Equal(t, "https://dz.example/1000.jpg", results[1].URL)
	require.Equal(t, 1000, results[1].Width)

	// the 100x100bb -> 1200x1200bb swap is undocumented, so assert it explicitly
	require.Equal(t, "https://is1.mzstatic.com/image/thumb/abc/1200x1200bb.jpg", results[2].URL)
	require.Equal(t, itunes.ArtworkLargeSize, results[2].Width)
}

func TestSearchSurvivesOneSourceFailing(t *testing.T) {
	t.Parallel()

	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(broken.Close)

	s := newSearcher(t)
	s.ITunes.BaseURL = broken.URL + "/"

	results, err := s.Search(context.Background(), Query{Artist: "Radiohead", Album: "OK Computer"})
	require.NoError(t, err, "one rate limited source must not fail the whole search")
	require.NotEmpty(t, results)
	for _, result := range results {
		require.NotEqual(t, SourceITunes, result.Source)
	}
}

func TestSearchRespectsSourceFilter(t *testing.T) {
	t.Parallel()
	s := newSearcher(t)

	results, err := s.Search(context.Background(), Query{
		Artist: "Radiohead", Album: "OK Computer", Sources: []string{"deezer"},
	})
	require.NoError(t, err)
	require.NotEmpty(t, results)
	for _, result := range results {
		require.Equal(t, SourceDeezer, result.Source)
	}
}

func TestSearchFallsBackToReleaseGroup(t *testing.T) {
	t.Parallel()

	// the release itself 404s, its group has the art
	caaServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/release-group/group-mbid" {
			_, _ = w.Write([]byte(caaReleaseJSON))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(caaServer.Close)

	mbServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"release-mbid","title":"OK Computer","release-group":{"id":"group-mbid"}}`))
	}))
	t.Cleanup(mbServer.Close)

	caa := coverartarchive.NewClient("gonic-test")
	caa.BaseURL = caaServer.URL + "/"
	caa.Limiter = noLimit()

	mb := musicbrainz.NewClient("gonic-test")
	mb.BaseURL = mbServer.URL + "/"
	mb.Limiter = noLimit()

	s := &Searcher{CoverArtArchive: caa, MusicBrainz: mb}
	results, err := s.Search(context.Background(), Query{MusicBrainzID: "release-mbid"})
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Equal(t, "https://ia.example/front-1200.jpg", results[0].URL)
}

func TestSearchNeedsSomethingToSearchFor(t *testing.T) {
	t.Parallel()
	s := newSearcher(t)

	_, err := s.Search(context.Background(), Query{})
	require.ErrorContains(t, err, "nothing to search for")
}

func TestSearchHonoursContextDeadline(t *testing.T) {
	t.Parallel()

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(slow.Close)

	dz := deezer.NewClient("gonic-test")
	dz.BaseURL = slow.URL + "/"
	dz.Limiter = noLimit()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	s := &Searcher{Deezer: dz}
	results, err := s.Search(ctx, Query{Text: "anything"})
	require.NoError(t, err)
	require.Empty(t, results)
}

// a typed query is how an admin says the album's tags are wrong. the archive arm must not
// keep answering from the mbid those tags carry.
func TestSearchTextOverridesTaggedMBID(t *testing.T) {
	t.Parallel()

	var caaPaths []string
	caaServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		caaPaths = append(caaPaths, r.URL.Path)
		_, _ = w.Write([]byte(caaReleaseJSON))
	}))
	t.Cleanup(caaServer.Close)

	var mbQuery string
	mbServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mbQuery = r.URL.Query().Get("query")
		_, _ = w.Write([]byte(`{"releases":[{"id":"searched-mbid","title":"Kid A",` +
			`"artist-credit":[{"name":"Radiohead"}]}]}`))
	}))
	t.Cleanup(mbServer.Close)

	caa := coverartarchive.NewClient("gonic-test")
	caa.BaseURL = caaServer.URL + "/"
	caa.Limiter = noLimit()

	mb := musicbrainz.NewClient("gonic-test")
	mb.BaseURL = mbServer.URL + "/"
	mb.Limiter = noLimit()

	s := &Searcher{CoverArtArchive: caa, MusicBrainz: mb}
	results, err := s.Search(context.Background(), Query{
		Artist: "Wrong Artist", Album: "Wrong Album", MusicBrainzID: "tagged-mbid", Text: "radiohead kid a",
	})
	require.NoError(t, err)
	require.NotEmpty(t, results)

	require.Equal(t, "radiohead kid a", mbQuery, "the typed text must reach musicbrainz verbatim")
	require.Equal(t, []string{"/release/searched-mbid"}, caaPaths, "the tagged mbid must not be used")

	// and the results are labelled with what was found, not with the album's own tags
	require.Equal(t, "Kid A", results[0].Title)
	require.Equal(t, "Radiohead", results[0].Artist)
}

// with no text typed, the tagged mbid is still the best answer
func TestSearchUsesTaggedMBIDWithoutText(t *testing.T) {
	t.Parallel()

	var caaPaths []string
	caaServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		caaPaths = append(caaPaths, r.URL.Path)
		_, _ = w.Write([]byte(caaReleaseJSON))
	}))
	t.Cleanup(caaServer.Close)

	caa := coverartarchive.NewClient("gonic-test")
	caa.BaseURL = caaServer.URL + "/"
	caa.Limiter = noLimit()

	s := &Searcher{CoverArtArchive: caa}
	results, err := s.Search(context.Background(), Query{
		Artist: "Radiohead", Album: "OK Computer", MusicBrainzID: "tagged-mbid",
	})
	require.NoError(t, err)
	require.NotEmpty(t, results)
	require.Equal(t, []string{"/release/tagged-mbid"}, caaPaths)
	require.Equal(t, "OK Computer", results[0].Title)
	require.Equal(t, "Radiohead", results[0].Artist)
}
