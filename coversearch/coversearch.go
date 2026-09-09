// Package coversearch fans an album out across the cover art sources and merges what comes
// back. it's the read side of cover art management: nothing here downloads or stores an
// image, it only finds candidate urls for a client to choose from.
package coversearch

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"go.senan.xyz/gonic/coverartarchive"
	"go.senan.xyz/gonic/deezer"
	"go.senan.xyz/gonic/itunes"
	"go.senan.xyz/gonic/musicbrainz"
)

// source names, also what a client passes in the `sources` param
const (
	SourceCoverArtArchive = "coverartarchive"
	SourceDeezer          = "deezer"
	SourceITunes          = "itunes"
)

// searches run concurrently but share a deadline. the musicbrainz limiter is one request a
// second and is shared with the artist info cache, so the archive arm is the slow one.
const searchTimeout = 20 * time.Second

const defaultLimit = 8

type Query struct {
	Artist        string
	Album         string
	MusicBrainzID string
	// Text overrides Artist/Album when a user types their own search
	Text    string
	Sources []string
	Limit   int
}

func (q Query) text() string {
	if q.Text != "" {
		return q.Text
	}
	return strings.TrimSpace(q.Artist + " " + q.Album)
}

func (q Query) wants(source string) bool {
	if len(q.Sources) == 0 {
		return true
	}
	for _, s := range q.Sources {
		if strings.EqualFold(strings.TrimSpace(s), source) {
			return true
		}
	}
	return false
}

func (q Query) limit() int {
	if q.Limit > 0 {
		return q.Limit
	}
	return defaultLimit
}

type Result struct {
	Source        string
	URL           string
	ThumbnailURL  string
	Title         string
	Artist        string
	Width, Height int
}

type Searcher struct {
	MusicBrainz     *musicbrainz.Client
	CoverArtArchive *coverartarchive.Client
	Deezer          *deezer.Client
	ITunes          *itunes.Client
}

// Search queries every enabled source concurrently. a source that fails is logged and
// skipped rather than failing the whole search: one rate limited third party shouldn't
// stop the other two being useful. results come back in a fixed source order.
func (s *Searcher) Search(ctx context.Context, q Query) ([]Result, error) {
	if s == nil {
		return nil, errors.New("cover search is not configured")
	}
	if q.text() == "" && q.MusicBrainzID == "" {
		return nil, errors.New("nothing to search for")
	}

	ctx, cancel := context.WithTimeout(ctx, searchTimeout)
	defer cancel()

	var caa, dz, it []Result
	var group errgroup.Group

	if s.CoverArtArchive != nil && q.wants(SourceCoverArtArchive) {
		group.Go(func() error {
			var err error
			if caa, err = s.searchCoverArtArchive(ctx, q); err != nil {
				log.Printf("cover search: %s: %v", SourceCoverArtArchive, err)
			}
			return nil
		})
	}
	if s.Deezer != nil && q.wants(SourceDeezer) {
		group.Go(func() error {
			var err error
			if dz, err = s.searchDeezer(ctx, q); err != nil {
				log.Printf("cover search: %s: %v", SourceDeezer, err)
			}
			return nil
		})
	}
	if s.ITunes != nil && q.wants(SourceITunes) {
		group.Go(func() error {
			var err error
			if it, err = s.searchITunes(ctx, q); err != nil {
				log.Printf("cover search: %s: %v", SourceITunes, err)
			}
			return nil
		})
	}

	_ = group.Wait() // no goroutine above returns an error

	results := make([]Result, 0, len(caa)+len(dz)+len(it))
	results = append(results, caa...)
	results = append(results, dz...)
	results = append(results, it...)
	return results, nil
}

func (s *Searcher) searchCoverArtArchive(ctx context.Context, q Query) ([]Result, error) {
	releases, err := s.coverArtArchiveReleases(ctx, q)
	if err != nil {
		return nil, err
	}

	var results []Result
	for _, release := range releases {
		for _, image := range release.Front() {
			results = append(results, Result{
				Source:       SourceCoverArtArchive,
				URL:          image.Image,
				ThumbnailURL: image.Thumbnail(),
				Title:        q.Album,
				Artist:       q.Artist,
			})
			if len(results) >= q.limit() {
				return results, nil
			}
		}
	}
	return results, nil
}

// coverArtArchiveReleases resolves a query to archive releases: straight to the mbid when
// the album is tagged with one, then its release group, then a musicbrainz search.
func (s *Searcher) coverArtArchiveReleases(ctx context.Context, q Query) ([]*coverartarchive.Release, error) {
	if q.MusicBrainzID != "" {
		return s.coverArtArchiveByMBID(ctx, q.MusicBrainzID)
	}

	if s.MusicBrainz == nil {
		return nil, nil
	}

	found, err := s.MusicBrainz.SearchReleases(ctx, musicBrainzQuery(q), 3)
	if err != nil {
		return nil, fmt.Errorf("search releases: %w", err)
	}

	var releases []*coverartarchive.Release
	for _, mbRelease := range found {
		release, err := s.CoverArtArchive.GetRelease(ctx, mbRelease.ID)
		if errors.Is(err, coverartarchive.ErrNotFound) {
			continue
		}
		if err != nil {
			return releases, err
		}
		releases = append(releases, release)
	}
	return releases, nil
}

// coverArtArchiveByMBID goes straight at the tagged release, then falls back to the
// release group it belongs to, which often carries art when a specific pressing doesn't.
func (s *Searcher) coverArtArchiveByMBID(ctx context.Context, mbid string) ([]*coverartarchive.Release, error) {
	release, err := s.CoverArtArchive.GetRelease(ctx, mbid)
	if err == nil {
		return []*coverartarchive.Release{release}, nil
	}
	if !errors.Is(err, coverartarchive.ErrNotFound) {
		return nil, err
	}
	if s.MusicBrainz == nil {
		return nil, nil
	}

	mbRelease, err := s.MusicBrainz.GetRelease(ctx, mbid, "release-groups")
	if err != nil || mbRelease.ReleaseGroup == nil {
		return nil, nil //nolint:nilerr // no release group is not an error, just no art
	}

	group, err := s.CoverArtArchive.GetReleaseGroup(ctx, mbRelease.ReleaseGroup.ID)
	if err == nil {
		return []*coverartarchive.Release{group}, nil
	}
	if !errors.Is(err, coverartarchive.ErrNotFound) {
		return nil, err
	}
	return nil, nil
}

func musicBrainzQuery(q Query) string {
	if q.Album == "" && q.Artist == "" {
		return q.text()
	}
	var parts []string
	if q.Album != "" {
		parts = append(parts, fmt.Sprintf("release:%q", q.Album))
	}
	if q.Artist != "" {
		parts = append(parts, fmt.Sprintf("artist:%q", q.Artist))
	}
	return strings.Join(parts, " AND ")
}

func (s *Searcher) searchDeezer(ctx context.Context, q Query) ([]Result, error) {
	albums, err := s.Deezer.SearchAlbums(ctx, q.text(), q.limit())
	if err != nil {
		return nil, err
	}
	results := make([]Result, 0, len(albums))
	for _, album := range albums {
		url := album.CoverXL
		width := 1000 // deezer's sizes are fixed by convention, not returned
		if url == "" {
			url, width = album.CoverBig, 500
		}
		if url == "" {
			continue
		}
		results = append(results, Result{
			Source:       SourceDeezer,
			URL:          url,
			ThumbnailURL: album.CoverMedium,
			Title:        album.Title,
			Artist:       album.Artist.Name,
			Width:        width,
			Height:       width,
		})
	}
	return results, nil
}

func (s *Searcher) searchITunes(ctx context.Context, q Query) ([]Result, error) {
	albums, err := s.ITunes.SearchAlbums(ctx, q.text(), q.limit())
	if err != nil {
		return nil, err
	}
	results := make([]Result, 0, len(albums))
	for _, album := range albums {
		url, highRes := album.HighResArtwork()
		if url == "" {
			continue
		}
		result := Result{
			Source:       SourceITunes,
			URL:          url,
			ThumbnailURL: album.ArtworkURL100,
			Title:        album.CollectionName,
			Artist:       album.ArtistName,
		}
		if highRes {
			result.Width, result.Height = itunes.ArtworkLargeSize, itunes.ArtworkLargeSize
		}
		results = append(results, result)
	}
	return results, nil
}
