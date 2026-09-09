// Package deezer is a client for Deezer's public search api. it needs no key.
package deezer

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"golang.org/x/time/rate"
)

const BaseURL = "https://api.deezer.com/"

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
		// deezer's documented soft limit is 50 requests per 5 seconds
		Limiter: rate.NewLimiter(rate.Every(120*time.Millisecond), 1),
	}
}

type Album struct {
	Title  string `json:"title"`
	Artist struct {
		Name string `json:"name"`
	} `json:"artist"`
	CoverMedium string `json:"cover_medium"`
	CoverBig    string `json:"cover_big"`
	CoverXL     string `json:"cover_xl"`
}

type searchResponse struct {
	Data []Album `json:"data"`
}

func (c *Client) SearchAlbums(ctx context.Context, query string, limit int) ([]Album, error) {
	if err := c.Limiter.Wait(ctx); err != nil {
		return nil, fmt.Errorf("rate limit: %w", err)
	}

	q := url.Values{}
	q.Set("q", query)
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}

	u, err := url.Parse(c.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse base url: %w", err)
	}
	u = u.JoinPath("search", "album")
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
	return sr.Data, nil
}
