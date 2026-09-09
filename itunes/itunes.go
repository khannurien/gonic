// Package itunes is a client for Apple's public iTunes Search api. it needs no key.
package itunes

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/time/rate"
)

const BaseURL = "https://itunes.apple.com/"

// the api only ever hands back a 100px url. swapping the size segment for a bigger one is
// undocumented, so HighResArtwork falls back to the original rather than erroring if the
// url ever stops matching this shape.
const (
	artworkSmallSegment = "100x100bb"
	artworkLargeSegment = "1200x1200bb"
	ArtworkLargeSize    = 1200
)

type StatusError int

func (s StatusError) Error() string { return fmt.Sprintf("bad status: %d", int(s)) }

type Client struct {
	BaseURL    string
	HTTPClient *http.Client
	UserAgent  string
	Limiter    *rate.Limiter
}

func NewClient(userAgent string) *Client {
	return &Client{
		BaseURL:    BaseURL,
		HTTPClient: &http.Client{Timeout: 15 * time.Second},
		UserAgent:  userAgent,
		// apple throttles around 20 requests a minute from one address
		Limiter: rate.NewLimiter(rate.Every(3*time.Second), 1),
	}
}

type Album struct {
	CollectionName string `json:"collectionName"`
	ArtistName     string `json:"artistName"`
	ArtworkURL100  string `json:"artworkUrl100"`
}

// HighResArtwork returns the large artwork url, or the 100px one if the url isn't the
// shape we expect.
func (a Album) HighResArtwork() (string, bool) {
	if !strings.Contains(a.ArtworkURL100, artworkSmallSegment) {
		return a.ArtworkURL100, false
	}
	return strings.Replace(a.ArtworkURL100, artworkSmallSegment, artworkLargeSegment, 1), true
}

type searchResponse struct {
	Results []Album `json:"results"`
}

func (c *Client) SearchAlbums(ctx context.Context, query string, limit int) ([]Album, error) {
	if err := c.Limiter.Wait(ctx); err != nil {
		return nil, fmt.Errorf("rate limit: %w", err)
	}

	q := url.Values{}
	q.Set("term", query)
	q.Set("entity", "album")
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}

	u, err := url.Parse(c.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse base url: %w", err)
	}
	u = u.JoinPath("search")
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("User-Agent", c.UserAgent)

	resp, err := c.HTTPClient.Do(req) //nolint:gosec // base url is ours
	if err != nil {
		return nil, fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		return nil, StatusError(resp.StatusCode)
	}

	var sr searchResponse
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return sr.Results, nil
}
