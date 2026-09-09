// Package coverartarchive is a client for the Cover Art Archive, the artwork companion to
// MusicBrainz. it's keyless, and keyed by release / release group mbid.
package coverartarchive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"golang.org/x/time/rate"
)

const BaseURL = "https://coverartarchive.org/"

var ErrNotFound = errors.New("not found")

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
		BaseURL: BaseURL,
		// not http.DefaultClient: that has no timeout, and this is a third party we don't
		// want to be able to hang a request on
		HTTPClient: &http.Client{Timeout: 15 * time.Second},
		UserAgent:  userAgent,
		Limiter:    rate.NewLimiter(rate.Every(time.Second), 1),
	}
}

// Image is one piece of art on a release. the archive 307s to archive.org for the actual
// bytes, which the default client follows.
type Image struct {
	Image      string            `json:"image"`
	Thumbnails map[string]string `json:"thumbnails"`
	Front      bool              `json:"front"`
	Back       bool              `json:"back"`
	Types      []string          `json:"types"`
}

// Thumbnail returns the smallest usable preview, falling back to the full image.
func (i Image) Thumbnail() string {
	for _, key := range []string{"250", "small", "500", "large"} {
		if url, ok := i.Thumbnails[key]; ok && url != "" {
			return url
		}
	}
	return i.Image
}

type Release struct {
	Images []Image `json:"images"`
}

// Front returns only the front covers, which is all that's useful as album art.
func (r Release) Front() []Image {
	var ret []Image
	for _, image := range r.Images {
		if image.Front {
			ret = append(ret, image)
		}
	}
	return ret
}

func (c *Client) GetRelease(ctx context.Context, mbid string) (*Release, error) {
	return c.get(ctx, "release", mbid)
}

func (c *Client) GetReleaseGroup(ctx context.Context, mbid string) (*Release, error) {
	return c.get(ctx, "release-group", mbid)
}

func (c *Client) get(ctx context.Context, entity, mbid string) (*Release, error) {
	if mbid == "" {
		return nil, ErrNotFound
	}
	if err := c.Limiter.Wait(ctx); err != nil {
		return nil, fmt.Errorf("rate limit: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+entity+"/"+mbid, nil)
	if err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("User-Agent", c.UserAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := c.HTTPClient.Do(req) //nolint:gosec // base url is ours
	if err != nil {
		return nil, fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if resp.StatusCode/100 != 2 {
		return nil, StatusError(resp.StatusCode)
	}

	var release Release
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return &release, nil
}
