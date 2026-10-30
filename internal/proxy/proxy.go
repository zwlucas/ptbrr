package proxy

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"ptbrr/internal/tmdb"
)

// TitleFetcher is an abstraction for querying localized movie/TV titles and search candidates.
type TitleFetcher interface {
	GetMovieTitle(ctx context.Context, tmdbID string) (string, error)
	TranslateByQuery(ctx context.Context, query string) (string, error)
	GetSearchCandidates(ctx context.Context, rawQuery, tmdbID string, isTV bool) ([]string, error)
}

// TorznabFeed represents the root RSS Torznab XML document.
type TorznabFeed struct {
	XMLName      xml.Name       `xml:"rss"`
	Version      string         `xml:"version,attr,omitempty"`
	XMLNSTorznab string         `xml:"xmlns:torznab,attr,omitempty"`
	XMLNSAtom    string         `xml:"xmlns:atom,attr,omitempty"`
	Channel      TorznabChannel `xml:"channel"`
}

// TorznabChannel represents the RSS channel element containing indexer metadata and items.
type TorznabChannel struct {
	XMLName     xml.Name      `xml:"channel"`
	Title       string        `xml:"title,omitempty"`
	Description string        `xml:"description,omitempty"`
	Link        string        `xml:"link,omitempty"`
	Language    string        `xml:"language,omitempty"`
	Category    string        `xml:"category,omitempty"`
	Items       []TorznabItem `xml:"item"`
}

// TorznabItem represents an individual torrent release in the Torznab feed.
// Using innerxml preserves all sub-elements (torznab:attr, enclosures, etc.) intact.
type TorznabItem struct {
	XMLName  xml.Name `xml:"item"`
	Guid     string   `xml:"guid"`
	Link     string   `xml:"link"`
	InnerXML []byte   `xml:",innerxml"`
}

// Proxy is a Torznab reverse proxy middleware that performs fan-out searches
// across multiple title variations (pt-BR official, alternatives, and original)
// and merges their XML responses into a unified, deduplicated feed.
type Proxy struct {
	upstreamURL  *url.URL
	titleFetcher TitleFetcher
	reverseProxy *httputil.ReverseProxy
	httpClient   *http.Client
	logger       *log.Logger
}

type responseRecorder struct {
	http.ResponseWriter
	statusCode int
}

func (rec *responseRecorder) WriteHeader(code int) {
	rec.statusCode = code
	rec.ResponseWriter.WriteHeader(code)
}

// NewProxy creates and initializes the reverse proxy middleware.
func NewProxy(upstreamURL *url.URL, titleFetcher TitleFetcher, logger *log.Logger) *Proxy {
	if logger == nil {
		logger = log.Default()
	}

	p := &Proxy{
		upstreamURL:  upstreamURL,
		titleFetcher: titleFetcher,
		logger:       logger,
		httpClient:   &http.Client{Timeout: 15 * time.Second},
	}

	revProxy := httputil.NewSingleHostReverseProxy(upstreamURL)

	originalDirector := revProxy.Director
	revProxy.Director = func(req *http.Request) {
		originalDirector(req)
		req.Host = upstreamURL.Host
		req.Header.Set("X-Forwarded-Host", req.Host)
		req.Header.Set("X-Forwarded-Proto", upstreamURL.Scheme)
	}

	revProxy.ErrorHandler = func(w http.ResponseWriter, req *http.Request, err error) {
		p.logger.Printf("[PROXY ERROR] Failed to connect to upstream Prowlarr (%s): %v", upstreamURL.String(), err)
		http.Error(w, fmt.Sprintf("502 Bad Gateway: Upstream Prowlarr (%s) is unreachable: %v", upstreamURL.String(), err), http.StatusBadGateway)
	}

	p.reverseProxy = revProxy
	return p
}

// ServeHTTP handles incoming requests.
// For search requests, it executes Fan-Out aggregation across title variations.
// For non-search requests (caps, ping, healthz), it passes through directly.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	// 1. Health check endpoint
	if r.URL.Path == "/healthz" || r.URL.Path == "/ping" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
		return
	}

	// 2. Parse query parameters
	queryParams := r.URL.Query()
	tmdbID := strings.TrimSpace(queryParams.Get("tmdbid"))
	rawQ := strings.TrimSpace(queryParams.Get("q"))
	tType := strings.TrimSpace(queryParams.Get("t"))
	catParam := strings.TrimSpace(queryParams.Get("cat"))

	// Check if this is a TV Show (Sonarr) or Movie (Radarr) request
	isTV := tmdb.IsTVRequest(catParam, tType, rawQ)
	isSearch := (tType == "search" || tType == "tvsearch" || tType == "movie" || rawQ != "" || tmdbID != "") && tType != "caps"

	// 3. For non-search requests (e.g., t=caps), pass through transparently
	if !isSearch || p.titleFetcher == nil {
		recorder := &responseRecorder{ResponseWriter: w, statusCode: http.StatusOK}
		p.reverseProxy.ServeHTTP(recorder, r)
		p.logger.Printf("[PROXY PASSTHROUGH] %s %s -> status=%d duration=%v", r.Method, r.URL.RequestURI(), recorder.statusCode, time.Since(start))
		return
	}

	mode := "Movie (Radarr)"
	if isTV {
		mode = "TV (Sonarr)"
	}
	p.logger.Printf("[ROUTING] Processing %s request: q=%q cat=%q t=%q", mode, rawQ, catParam, tType)

	// 4. Resolve candidate search titles from TMDB (Exclusively Portuguese titles: Official pt-BR and Alternatives)
	candidates, err := p.titleFetcher.GetSearchCandidates(r.Context(), rawQ, tmdbID, isTV)
	if err != nil {
		p.logger.Printf("[PROXY WARNING] Failed to get %s TMDB candidates for q=%q tmdbid=%s: %v", mode, rawQ, tmdbID, err)
	}

	// Rule 2: Strictly return Portuguese titles. If none found, return empty feed without querying English fallback.
	if len(candidates) == 0 {
		p.logger.Printf("[PROXY] No Portuguese titles found for q=%q tmdbid=%s. Returning empty feed (English fallback disabled).", rawQ, tmdbID)
		emptyFeed := &TorznabFeed{
			Version:      "2.0",
			XMLNSTorznab: "http://torznab.com/schemas/2015/feed",
			XMLNSAtom:    "http://www.w3.org/2005/Atom",
			Channel: TorznabChannel{
				Title: "Prowlarr",
				Items: []TorznabItem{},
			},
		}
		w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(xml.Header))
		_ = xml.NewEncoder(w).Encode(emptyFeed)
		return
	}

	p.logger.Printf("[FAN-OUT START] Initiating %d concurrent Prowlarr searches: %v", len(candidates), candidates)

	// 5. Fan-Out: Execute concurrent queries to Prowlarr for each candidate title
	type queryResult struct {
		query string
		feed  *TorznabFeed
		err   error
	}

	resultsChan := make(chan queryResult, len(candidates))
	var wg sync.WaitGroup

	for _, cand := range candidates {
		wg.Add(1)
		go func(titleQuery string) {
			defer wg.Done()
			// Rule 3: Individual log inside goroutine
			p.logger.Printf("[FAN-OUT] Executing GET for: %q", titleQuery)
			feed, err := p.fetchProwlarrFeed(r.Context(), r, titleQuery)
			resultsChan <- queryResult{query: titleQuery, feed: feed, err: err}
		}(cand)
	}

	wg.Wait()
	close(resultsChan)

	// 6. Collect successful responses
	var successfulFeeds []*TorznabFeed
	for res := range resultsChan {
		if res.err != nil {
			p.logger.Printf("[FAN-OUT WARNING] Upstream query %q failed: %v", res.query, res.err)
			continue
		}
		if res.feed != nil {
			successfulFeeds = append(successfulFeeds, res.feed)
		}
	}

	// If all requests failed, return 502 Bad Gateway
	if len(successfulFeeds) == 0 {
		p.logger.Printf("[FAN-OUT ERROR] All %d upstream Prowlarr queries failed", len(candidates))
		http.Error(w, "502 Bad Gateway: All upstream Prowlarr queries failed", http.StatusBadGateway)
		return
	}

	// 7. Merge feeds and deduplicate items based on <guid> or <link>
	mergedFeed, totalCount, dupsRemoved := mergeTorznabFeeds(successfulFeeds)
	p.logger.Printf("[FAN-OUT MERGE] Merged %d total items (%d duplicates removed) -> %d unique items returned in %v",
		totalCount, dupsRemoved, len(mergedFeed.Channel.Items), time.Since(start))

	// 8. Output unified XML to Radarr
	w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(xml.Header))

	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	if err := enc.Encode(mergedFeed); err != nil {
		p.logger.Printf("[PROXY ERROR] Failed to encode merged Torznab XML: %v", err)
	}
}

// fetchProwlarrFeed executes an HTTP request to upstream Prowlarr with the modified query parameter.
func (p *Proxy) fetchProwlarrFeed(ctx context.Context, originalReq *http.Request, queryParam string) (*TorznabFeed, error) {
	targetURL := *p.upstreamURL
	targetURL.Path = singleJoiningSlash(targetURL.Path, originalReq.URL.Path)

	q := originalReq.URL.Query()
	q.Set("q", queryParam)
	targetURL.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create upstream request: %w", err)
	}

	// Copy important headers from Radarr request (API Key, User-Agent, Accept, etc.)
	for k, vv := range originalReq.Header {
		lowerK := strings.ToLower(k)
		if lowerK == "accept" || lowerK == "user-agent" || lowerK == "authorization" || strings.HasPrefix(lowerK, "x-") {
			for _, v := range vv {
				req.Header.Add(k, v)
			}
		}
	}
	req.Host = p.upstreamURL.Host

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upstream request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("upstream HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}

	var feed TorznabFeed
	if err := xml.NewDecoder(resp.Body).Decode(&feed); err != nil {
		return nil, fmt.Errorf("failed to decode Torznab XML: %w", err)
	}

	return &feed, nil
}

// mergeTorznabFeeds merges items from multiple feeds and removes duplicates based on GUID or Link.
func mergeTorznabFeeds(feeds []*TorznabFeed) (merged *TorznabFeed, totalItems, duplicatesRemoved int) {
	if len(feeds) == 0 {
		return &TorznabFeed{
			Version:      "2.0",
			XMLNSTorznab: "http://torznab.com/schemas/2015/feed",
			XMLNSAtom:    "http://www.w3.org/2005/Atom",
		}, 0, 0
	}

	baseFeed := *feeds[0]
	if baseFeed.Version == "" {
		baseFeed.Version = "2.0"
	}
	if baseFeed.XMLNSTorznab == "" {
		baseFeed.XMLNSTorznab = "http://torznab.com/schemas/2015/feed"
	}
	if baseFeed.XMLNSAtom == "" {
		baseFeed.XMLNSAtom = "http://www.w3.org/2005/Atom"
	}

	seenKeys := make(map[string]bool)
	var uniqueItems []TorznabItem

	for _, feed := range feeds {
		for _, item := range feed.Channel.Items {
			totalItems++

			dedupKey := strings.TrimSpace(item.Guid)
			if dedupKey == "" {
				dedupKey = strings.TrimSpace(item.Link)
			}

			if dedupKey != "" && seenKeys[dedupKey] {
				duplicatesRemoved++
				continue
			}

			if dedupKey != "" {
				seenKeys[dedupKey] = true
			}
			uniqueItems = append(uniqueItems, item)
		}
	}

	baseFeed.Channel.Items = uniqueItems
	return &baseFeed, totalItems, duplicatesRemoved
}

func singleJoiningSlash(a, b string) string {
	aslash := strings.HasSuffix(a, "/")
	bslash := strings.HasPrefix(b, "/")
	switch {
	case aslash && bslash:
		return a + b[1:]
	case !aslash && !bslash:
		return a + "/" + b
	}
	return a + b
}
