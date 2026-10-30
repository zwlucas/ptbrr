package tmdb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	// ErrMissingAPIKey is returned when TMDB_API_KEY is not configured.
	ErrMissingAPIKey = errors.New("TMDB API key is not configured")
	// ErrEmptyTMDBID is returned when the provided tmdbid parameter is empty.
	ErrEmptyTMDBID = errors.New("tmdbid cannot be empty")
	// ErrEmptyQuery is returned when the search query is empty.
	ErrEmptyQuery = errors.New("query cannot be empty")
	// ErrTitleNotFound is returned when TMDB returns a 200 OK but the title field is empty.
	ErrTitleNotFound = errors.New("title not found in TMDB response")
	// ErrNoSearchResults is returned when TMDB search/movie returns zero results.
	ErrNoSearchResults = errors.New("no movie results found in TMDB")
)

// trailingYearRegex matches a 4-digit release year at the end of the query (e.g., " 1999" or " (1999)").
var trailingYearRegex = regexp.MustCompile(`(?:\s+|\()(\b(?:19|20)\d{2}\b)\)?\s*$`)

// punctuationRegex matches punctuation characters that break tracker searches (:, -, ,, ., !, ?, _, ', etc.).
var punctuationRegex = regexp.MustCompile(`[:,\-–—\.\!\?_\'\"/\\()\[\]{}~]`)
var multiSpaceRegex = regexp.MustCompile(`\s+`)

// CleanTitle removes punctuation and normalizes multiple spaces into a single space.
func CleanTitle(title string) string {
	cleaned := punctuationRegex.ReplaceAllString(title, " ")
	cleaned = multiSpaceRegex.ReplaceAllString(cleaned, " ")
	return strings.TrimSpace(cleaned)
}

// tvSuffixRegex captures season/episode patterns at the end of TV queries (e.g., "S01E01", "S01", "Season 1", "Temporada 2").
var tvSuffixRegex = regexp.MustCompile(`(?i)\s+((?:S\d{1,4}(?:E\d{1,4}(?:-E\d{1,4})?)?|(?:Season|Temporada)\s+\d{1,2}))\s*$`)

// IsTVRequest inspects Torznab category (cat), query type (t), and rawQuery.
// Categories in the 5000s (e.g. 5000, 5030, 5040) are Sonarr / TV Shows.
// Categories in the 2000s (e.g. 2000, 2030, 2040) are Radarr / Movies.
func IsTVRequest(catParam, tParam, rawQuery string) bool {
	if tParam == "tvsearch" {
		return true
	}
	if tParam == "movie" {
		return false
	}

	for _, part := range strings.Split(catParam, ",") {
		part = strings.TrimSpace(part)
		if len(part) >= 4 {
			if strings.HasPrefix(part, "5") {
				return true
			}
			if strings.HasPrefix(part, "2") {
				return false
			}
		}
	}

	// Fallback detection: if query contains TV season/episode patterns (e.g. S01E01, Season 1)
	_, _, suffix := ExtractTVQueryAndSuffix(rawQuery)
	return suffix != ""
}

// ExtractTVQueryAndSuffix extracts the base series name, optional year, and season/episode suffix.
// Examples:
// "Breaking Bad S01E01" -> ("Breaking Bad", "", "S01E01")
// "Chernobyl S01" -> ("Chernobyl", "", "S01")
// "The Last of Us Season 1" -> ("The Last of Us", "", "Season 1")
// "Dark 2017 S01" -> ("Dark", "2017", "S01")
func ExtractTVQueryAndSuffix(rawQuery string) (baseName, year, suffix string) {
	trimmed := strings.TrimSpace(rawQuery)
	matches := tvSuffixRegex.FindStringSubmatch(trimmed)
	basePart := trimmed
	if len(matches) == 2 {
		suffix = strings.TrimSpace(matches[1])
		basePart = strings.TrimSpace(tvSuffixRegex.ReplaceAllString(trimmed, ""))
	}

	cleanBase, yr := extractYearAndCleanTitle(basePart)
	if cleanBase == "" {
		cleanBase = basePart
	}
	return cleanBase, yr, suffix
}

type cacheEntry struct {
	title     string
	expiresAt time.Time
}

type candidatesCacheEntry struct {
	titles    []string
	expiresAt time.Time
}

// Client interacts with the TMDB API and caches responses in memory.
type Client struct {
	apiKey     string
	baseURL    string
	httpClient *http.Client
	cacheTTL   time.Duration

	mu              sync.RWMutex
	cache           map[string]cacheEntry
	candidatesCache map[string]candidatesCacheEntry
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
		apiKey:          apiKey,
		baseURL:         baseURL,
		httpClient:      &http.Client{Timeout: timeout},
		cacheTTL:        cacheTTL,
		cache:           make(map[string]cacheEntry),
		candidatesCache: make(map[string]candidatesCacheEntry),
	}
}

// movieResponse matches the relevant fields in TMDB /movie/{tmdbid} JSON response.
type movieResponse struct {
	ID            int64  `json:"id"`
	Title         string `json:"title"`
	OriginalTitle string `json:"original_title"`
}

// searchMovieResponse matches the TMDB /search/movie JSON response.
type searchMovieResponse struct {
	Page    int `json:"page"`
	Results []struct {
		ID            int64  `json:"id"`
		Title         string `json:"title"`
		OriginalTitle string `json:"original_title"`
		ReleaseDate   string `json:"release_date"`
	} `json:"results"`
	TotalResults int `json:"total_results"`
}

// alternativeTitlesResponse matches TMDB /movie/{tmdbid}/alternative_titles JSON response.
type alternativeTitlesResponse struct {
	ID     int64 `json:"id"`
	Titles []struct {
		ISO3166_1 string `json:"iso_3166_1"`
		Title     string `json:"title"`
		Type      string `json:"type"`
	} `json:"titles"`
}

// searchTVResponse matches TMDB /search/tv JSON response.
type searchTVResponse struct {
	Page    int `json:"page"`
	Results []struct {
		ID           int64  `json:"id"`
		Name         string `json:"name"`
		OriginalName string `json:"original_name"`
	} `json:"results"`
	TotalResults int `json:"total_results"`
}

// tvDetailsResponse matches TMDB /tv/{id} JSON response.
type tvDetailsResponse struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	OriginalName string `json:"original_name"`
}

// tvAlternativeTitlesResponse matches TMDB /tv/{id}/alternative_titles JSON response.
type tvAlternativeTitlesResponse struct {
	ID      int64 `json:"id"`
	Results []struct {
		ISO3166_1 string `json:"iso_3166_1"`
		Title     string `json:"title"`
	} `json:"results"`
}

// GetTVShowTitle fetches the localized pt-BR name for a given TV show ID.
func (c *Client) GetTVShowTitle(ctx context.Context, tvID string) (string, error) {
	tvID = strings.TrimSpace(tvID)
	if tvID == "" {
		return "", ErrEmptyTMDBID
	}
	if title, found := c.getFromCache("tv:" + tvID); found {
		return title, nil
	}
	if c.apiKey == "" {
		return "", ErrMissingAPIKey
	}

	endpoint := fmt.Sprintf("%s/tv/%s?api_key=%s&language=pt-BR",
		c.baseURL,
		url.PathEscape(tvID),
		url.QueryEscape(c.apiKey),
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "ptbrr-proxy/1.0")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("TMDB TV returned HTTP %d", resp.StatusCode)
	}

	var tv tvDetailsResponse
	if err := json.NewDecoder(resp.Body).Decode(&tv); err != nil {
		return "", err
	}

	name := strings.TrimSpace(tv.Name)
	if name == "" {
		return "", ErrTitleNotFound
	}

	c.setInCache("tv:"+tvID, name)
	return name, nil
}

// GetTVAlternativeTitles fetches alternative titles for a TV show ID, filtering for BR and PT.
func (c *Client) GetTVAlternativeTitles(ctx context.Context, tvID string) ([]string, error) {
	tvID = strings.TrimSpace(tvID)
	if tvID == "" {
		return nil, ErrEmptyTMDBID
	}
	if c.apiKey == "" {
		return nil, ErrMissingAPIKey
	}

	endpoint := fmt.Sprintf("%s/tv/%s/alternative_titles?api_key=%s",
		c.baseURL,
		url.PathEscape(tvID),
		url.QueryEscape(c.apiKey),
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "ptbrr-proxy/1.0")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("TMDB TV alternative_titles returned HTTP %d", resp.StatusCode)
	}

	var altResp tvAlternativeTitlesResponse
	if err := json.NewDecoder(resp.Body).Decode(&altResp); err != nil {
		return nil, err
	}

	var titles []string
	for _, t := range altResp.Results {
		iso := strings.ToUpper(strings.TrimSpace(t.ISO3166_1))
		cleanT := strings.TrimSpace(t.Title)
		if (iso == "BR" || iso == "PT") && cleanT != "" {
			titles = append(titles, cleanT)
		}
	}
	return titles, nil
}

// GetTVSearchCandidates searches TMDB TV, fetches pt-BR & alternative titles, sanitizes with CleanTitle,
// and re-attaches the exact season/episode suffix (e.g. "S01E01").
func (c *Client) GetTVSearchCandidates(ctx context.Context, rawQuery, tmdbID string) ([]string, error) {
	rawQuery = strings.TrimSpace(rawQuery)
	tmdbID = strings.TrimSpace(tmdbID)
	baseName, year, suffix := ExtractTVQueryAndSuffix(rawQuery)

	tvID := tmdbID
	if tvID == "" && baseName != "" {
		searchURL := fmt.Sprintf("%s/search/tv?api_key=%s&query=%s",
			c.baseURL,
			url.QueryEscape(c.apiKey),
			url.QueryEscape(baseName),
		)
		if year != "" {
			searchURL += fmt.Sprintf("&first_air_date_year=%s", url.QueryEscape(year))
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, searchURL, nil)
		if err == nil {
			req.Header.Set("Accept", "application/json")
			req.Header.Set("User-Agent", "ptbrr-proxy/1.0")
			if resp, err := c.httpClient.Do(req); err == nil {
				defer resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					var sResp searchTVResponse
					if err := json.NewDecoder(resp.Body).Decode(&sResp); err == nil && len(sResp.Results) > 0 {
						tvID = strconv.FormatInt(sResp.Results[0].ID, 10)
					}
				}
			}
		}
	}

	if tvID == "" {
		return nil, errors.New("could not identify TV show from query")
	}

	// Cache check
	cacheKey := fmt.Sprintf("tvcands:%s:%s", tvID, suffix)
	c.mu.RLock()
	entry, found := c.candidatesCache[cacheKey]
	c.mu.RUnlock()
	if found && time.Now().Before(entry.expiresAt) {
		return entry.titles, nil
	}

	// Concurrently fetch official pt-BR title and alternative titles
	var officialTitle string
	var altTitles []string
	var wg sync.WaitGroup

	wg.Add(2)
	go func() {
		defer wg.Done()
		if t, err := c.GetTVShowTitle(ctx, tvID); err == nil && t != "" {
			officialTitle = t
		}
	}()

	go func() {
		defer wg.Done()
		if alts, err := c.GetTVAlternativeTitles(ctx, tvID); err == nil {
			altTitles = alts
		}
	}()

	wg.Wait()

	seen := make(map[string]bool)
	var candidates []string

	addCandidate := func(title string) {
		clean := CleanTitle(title)
		if clean == "" {
			return
		}
		if suffix != "" {
			clean = fmt.Sprintf("%s %s", clean, suffix)
		}
		normalized := strings.ToLower(clean)
		if !seen[normalized] {
			seen[normalized] = true
			candidates = append(candidates, clean)
		}
	}

	if officialTitle != "" {
		addCandidate(officialTitle)
	}
	for _, alt := range altTitles {
		addCandidate(alt)
	}

	c.mu.Lock()
	c.candidatesCache[cacheKey] = candidatesCacheEntry{
		titles:    candidates,
		expiresAt: time.Now().Add(c.cacheTTL),
	}
	c.mu.Unlock()

	return candidates, nil
}

// GetAlternativeTitles fetches Portuguese alternative titles (iso_3166_1: "BR" or "PT") for a given movie ID.
func (c *Client) GetAlternativeTitles(ctx context.Context, movieID string) ([]string, error) {
	movieID = strings.TrimSpace(movieID)
	if movieID == "" {
		return nil, ErrEmptyTMDBID
	}
	if c.apiKey == "" {
		return nil, ErrMissingAPIKey
	}

	endpoint := fmt.Sprintf("%s/movie/%s/alternative_titles?api_key=%s",
		c.baseURL,
		url.PathEscape(movieID),
		url.QueryEscape(c.apiKey),
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create alternative titles request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "ptbrr-proxy/1.0")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("alternative titles request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("TMDB alternative titles returned HTTP %d", resp.StatusCode)
	}

	var altResp alternativeTitlesResponse
	if err := json.NewDecoder(resp.Body).Decode(&altResp); err != nil {
		return nil, fmt.Errorf("failed to decode alternative titles JSON: %w", err)
	}

	var titles []string
	for _, t := range altResp.Titles {
		iso := strings.ToUpper(strings.TrimSpace(t.ISO3166_1))
		cleanT := strings.TrimSpace(t.Title)
		if (iso == "BR" || iso == "PT") && cleanT != "" {
			titles = append(titles, cleanT)
		}
	}

	return titles, nil
}

// GetSearchCandidates routes the request to TV or Movie candidate resolver based on isTV.
func (c *Client) GetSearchCandidates(ctx context.Context, rawQuery, tmdbID string, isTV bool) ([]string, error) {
	if isTV {
		return c.GetTVSearchCandidates(ctx, rawQuery, tmdbID)
	}
	return c.GetMovieSearchCandidates(ctx, rawQuery, tmdbID)
}

// GetMovieSearchCandidates resolves the movie ID and fetches:
// 1. The official localized pt-BR title
// 2. Alternative titles for BR and PT
func (c *Client) GetMovieSearchCandidates(ctx context.Context, rawQuery, tmdbID string) ([]string, error) {
	rawQuery = strings.TrimSpace(rawQuery)
	tmdbID = strings.TrimSpace(tmdbID)
	cleanTitle, year := extractYearAndCleanTitle(rawQuery)

	movieID := tmdbID
	if movieID == "" && cleanTitle != "" {
		// Resolve movie ID by querying /search/movie
		searchURL := fmt.Sprintf("%s/search/movie?api_key=%s&query=%s",
			c.baseURL,
			url.QueryEscape(c.apiKey),
			url.QueryEscape(cleanTitle),
		)
		if year != "" {
			searchURL += fmt.Sprintf("&year=%s", url.QueryEscape(year))
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, searchURL, nil)
		if err == nil {
			req.Header.Set("Accept", "application/json")
			req.Header.Set("User-Agent", "ptbrr-proxy/1.0")
			if resp, err := c.httpClient.Do(req); err == nil {
				defer resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					var sResp searchMovieResponse
					if err := json.NewDecoder(resp.Body).Decode(&sResp); err == nil && len(sResp.Results) > 0 {
						movieID = strconv.FormatInt(sResp.Results[0].ID, 10)
					}
				}
			}
		}
	}

	// If no movie ID could be found, return the raw query as fallback
	if movieID == "" {
		if rawQuery != "" {
			return []string{rawQuery}, nil
		}
		return nil, errors.New("could not identify movie from query")
	}

	// Check candidate cache
	cacheKey := fmt.Sprintf("candidates:%s:%s", movieID, year)
	c.mu.RLock()
	entry, found := c.candidatesCache[cacheKey]
	c.mu.RUnlock()
	if found && time.Now().Before(entry.expiresAt) {
		return entry.titles, nil
	}

	// Concurrently fetch official pt-BR title and alternative titles
	var officialTitle string
	var altTitles []string
	var wg sync.WaitGroup

	wg.Add(2)
	go func() {
		defer wg.Done()
		if t, err := c.GetMovieTitle(ctx, movieID); err == nil && t != "" {
			officialTitle = t
		}
	}()

	go func() {
		defer wg.Done()
		if alts, err := c.GetAlternativeTitles(ctx, movieID); err == nil {
			altTitles = alts
		}
	}()

	wg.Wait()

	// Assemble and deduplicate clean, sanitized Portuguese title candidate list (NO punctuation, NO year, NO English fallback)
	seen := make(map[string]bool)
	var candidates []string

	addCandidate := func(title string) {
		clean := CleanTitle(title)
		if clean == "" {
			return
		}
		normalized := strings.ToLower(clean)
		if !seen[normalized] {
			seen[normalized] = true
			candidates = append(candidates, clean)
		}
	}

	// 1. Official pt-BR title (sanitized without punctuation)
	if officialTitle != "" {
		addCandidate(officialTitle)
	}

	// 2. Alternative BR / PT titles (sanitized without punctuation)
	for _, alt := range altTitles {
		addCandidate(alt)
	}

	// Cache candidates
	c.mu.Lock()
	c.candidatesCache[cacheKey] = candidatesCacheEntry{
		titles:    candidates,
		expiresAt: time.Now().Add(c.cacheTTL),
	}
	c.mu.Unlock()

	return candidates, nil
}

// extractYearAndCleanTitle extracts the 4-digit year at the end of a title query if present.
func extractYearAndCleanTitle(rawQuery string) (cleanTitle, year string) {
	trimmed := strings.TrimSpace(rawQuery)
	matches := trailingYearRegex.FindStringSubmatch(trimmed)
	if len(matches) == 2 {
		year = matches[1]
		clean := strings.TrimSpace(trailingYearRegex.ReplaceAllString(trimmed, ""))
		if clean != "" {
			return clean, year
		}
	}
	return trimmed, ""
}

// TranslateByQuery performs text-based reverse search for Torznab indexers that don't provide tmdbid.
// It searches TMDB for the movie, captures the first ID, fetches the localized pt-BR title,
// and appends the original year if present.
func (c *Client) TranslateByQuery(ctx context.Context, rawQuery string) (string, error) {
	rawQuery = strings.TrimSpace(rawQuery)
	if rawQuery == "" {
		return "", ErrEmptyQuery
	}

	cacheKey := "q:" + strings.ToLower(rawQuery)
	if cachedTitle, found := c.getFromCache(cacheKey); found {
		return cachedTitle, nil
	}

	if c.apiKey == "" {
		return "", ErrMissingAPIKey
	}

	cleanTitle, year := extractYearAndCleanTitle(rawQuery)

	// Step 1: Query TMDB Search API: /search/movie?api_key={key}&query={cleanTitle}&year={year}
	searchURL := fmt.Sprintf("%s/search/movie?api_key=%s&query=%s",
		c.baseURL,
		url.QueryEscape(c.apiKey),
		url.QueryEscape(cleanTitle),
	)
	if year != "" {
		searchURL += fmt.Sprintf("&year=%s", url.QueryEscape(year))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, searchURL, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create TMDB search request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "ptbrr-proxy/1.0")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("TMDB search request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodySnippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("TMDB search returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(bodySnippet)))
	}

	var searchResp searchMovieResponse
	if err := json.NewDecoder(resp.Body).Decode(&searchResp); err != nil {
		return "", fmt.Errorf("failed to decode TMDB search JSON: %w", err)
	}

	if len(searchResp.Results) == 0 {
		// If year search gave 0 results, retry without year parameter before giving up
		if year != "" {
			return c.searchWithoutYear(ctx, cleanTitle, rawQuery, year)
		}
		return "", ErrNoSearchResults
	}

	// Step 2: Grab the first result's ID and fetch localized pt-BR details
	firstMovieID := strconv.FormatInt(searchResp.Results[0].ID, 10)
	ptBRTitle, err := c.GetMovieTitle(ctx, firstMovieID)
	if err != nil {
		return "", fmt.Errorf("failed to get pt-BR details for movie ID %s: %w", firstMovieID, err)
	}

	// Step 3: Append original year if present
	finalTitle := ptBRTitle
	if year != "" {
		finalTitle = fmt.Sprintf("%s %s", ptBRTitle, year)
	}

	// Cache the query mapping
	c.setInCache(cacheKey, finalTitle)

	return finalTitle, nil
}

func (c *Client) searchWithoutYear(ctx context.Context, cleanTitle, rawQuery, year string) (string, error) {
	searchURL := fmt.Sprintf("%s/search/movie?api_key=%s&query=%s",
		c.baseURL,
		url.QueryEscape(c.apiKey),
		url.QueryEscape(cleanTitle),
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, searchURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "ptbrr-proxy/1.0")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", ErrNoSearchResults
	}

	var searchResp searchMovieResponse
	if err := json.NewDecoder(resp.Body).Decode(&searchResp); err != nil || len(searchResp.Results) == 0 {
		return "", ErrNoSearchResults
	}

	firstMovieID := strconv.FormatInt(searchResp.Results[0].ID, 10)
	ptBRTitle, err := c.GetMovieTitle(ctx, firstMovieID)
	if err != nil {
		return "", err
	}

	finalTitle := fmt.Sprintf("%s %s", ptBRTitle, year)
	c.setInCache("q:"+strings.ToLower(rawQuery), finalTitle)
	return finalTitle, nil
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

func (c *Client) getFromCache(key string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	entry, ok := c.cache[key]
	if !ok {
		return "", false
	}

	if time.Now().After(entry.expiresAt) {
		return "", false
	}

	return entry.title, true
}

func (c *Client) setInCache(key, title string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.cache[key] = cacheEntry{
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
