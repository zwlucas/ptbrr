package tmdb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

var (
	// ErrMissingAPIKey is returned when TMDB_API_KEY is not configured.
	ErrMissingAPIKey = errors.New("TMDB API key is not configured")
	// ErrEmptyTMDBID is returned when the provided tmdbid parameter is empty.
	ErrEmptyTMDBID = errors.New("tmdbid cannot be empty")
	// ErrTitleNotFound is returned when TMDB returns a 200 OK but the title field is empty.
	ErrTitleNotFound = errors.New("title not found in TMDB response")
)

type cacheEntry struct {
	title     string
	expiresAt time.Time
}

// Client interacts with the TMDB API and caches responses in memory.
type Client struct {
	apiKey     string
	baseURL    string
	httpClient *http.Client
	cacheTTL   time.Duration

	mu    sync.RWMutex
	cache map[string]cacheEntry
}

// NewClient initializes a new TMDB client.
func NewClient(apiKey, baseURL string, timeout, cacheTTL time.Duration) *Client {
	if baseURL == "" {
		baseURL = "https://api.themoviedb.org/3"
	}
	baseURL = strings.TrimRight(baseURL, "/")

	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	if cacheTTL <= 0 {
		cacheTTL = 24 * time.Hour
	}

	return &Client{
		apiKey:     apiKey,
		baseURL:    baseURL,
		httpClient: &http.Client{Timeout: timeout},
		cacheTTL:   cacheTTL,
		cache:      make(map[string]cacheEntry),
	}
}

// movieResponse matches the relevant fields in TMDB /movie/{tmdbid} JSON response.
type movieResponse struct {
	ID            int64  `json:"id"`
	Title         string `json:"title"`
	OriginalTitle string `json:"original_title"`
}

// GetMovieTitle fetches the Brazilian Portuguese (pt-BR) localized title for a given tmdbid.
// It checks the in-memory cache before making any outbound HTTP request.
func (c *Client) GetMovieTitle(ctx context.Context, tmdbID string) (string, error) {
	tmdbID = strings.TrimSpace(tmdbID)
	if tmdbID == "" {
		return "", ErrEmptyTMDBID
	}

	// 1. Check in-memory cache
	if title, found := c.getFromCache(tmdbID); found {
		return title, nil
	}

	// 2. Validate API key
	if c.apiKey == "" {
		return "", ErrMissingAPIKey
	}

	// 3. Build TMDB request URL:
	// https://api.themoviedb.org/3/movie/{tmdbid}?api_key={TMDB_API_KEY}&language=pt-BR
	endpoint := fmt.Sprintf("%s/movie/%s?api_key=%s&language=pt-BR",
		c.baseURL,
		url.PathEscape(tmdbID),
		url.QueryEscape(c.apiKey),
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create TMDB request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "ptbrr-proxy/1.0")

	// 4. Execute request
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("TMDB request failed: %w", err)
	}
	defer resp.Body.Close()

	// 5. Handle non-200 responses
	if resp.StatusCode != http.StatusOK {
		bodySnippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("TMDB returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(bodySnippet)))
	}

	// 6. Decode JSON response
	var movie movieResponse
	if err := json.NewDecoder(resp.Body).Decode(&movie); err != nil {
		return "", fmt.Errorf("failed to decode TMDB response JSON: %w", err)
	}

	title := strings.TrimSpace(movie.Title)
	if title == "" {
		return "", ErrTitleNotFound
	}

	// 7. Save to cache
	c.setInCache(tmdbID, title)

	return title, nil
}

func (c *Client) getFromCache(tmdbID string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	entry, ok := c.cache[tmdbID]
	if !ok {
		return "", false
	}

	if time.Now().After(entry.expiresAt) {
		return "", false
	}

	return entry.title, true
}

func (c *Client) setInCache(tmdbID, title string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.cache[tmdbID] = cacheEntry{
		title:     title,
		expiresAt: time.Now().Add(c.cacheTTL),
	}
}

// CacheCount returns the current count of cached items.
func (c *Client) CacheCount() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.cache)
}

// PurgeExpired cleans up expired entries from the cache.
func (c *Client) PurgeExpired() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	purged := 0
	for id, entry := range c.cache {
		if now.After(entry.expiresAt) {
			delete(c.cache, id)
			purged++
		}
	}
	return purged
}
