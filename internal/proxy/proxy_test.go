package proxy

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

type mockTitleFetcher struct {
	titles     map[string]string
	queries    map[string]string
	candidates map[string][]string
	err        error
}

func (m *mockTitleFetcher) GetMovieTitle(ctx context.Context, tmdbID string) (string, error) {
	if m.err != nil {
		return "", m.err
	}
	if title, ok := m.titles[tmdbID]; ok {
		return title, nil
	}
	return "", fmt.Errorf("tmdbid %s not found in mock", tmdbID)
}

func (m *mockTitleFetcher) TranslateByQuery(ctx context.Context, query string) (string, error) {
	if m.err != nil {
		return "", m.err
	}
	if title, ok := m.queries[query]; ok {
		return title, nil
	}
	return "", fmt.Errorf("query %q not found in mock", query)
}

func (m *mockTitleFetcher) GetSearchCandidates(ctx context.Context, rawQuery, tmdbID string, isTV bool) ([]string, error) {
	if m.err != nil {
		return nil, m.err
	}
	if list, ok := m.candidates[rawQuery]; ok {
		return list, nil
	}
	if list, ok := m.candidates[tmdbID]; ok {
		return list, nil
	}
	if title, ok := m.queries[rawQuery]; ok {
		return []string{title}, nil
	}
	if title, ok := m.titles[tmdbID]; ok {
		return []string{title}, nil
	}
	if rawQuery != "" {
		return []string{rawQuery}, nil
	}
	return nil, fmt.Errorf("no candidates found in mock for query %q / id %q", rawQuery, tmdbID)
}

func TestProxy_TranslationSuccess(t *testing.T) {
	const sampleXML = `<?xml version="1.0" encoding="utf-8"?><rss version="2.0"><channel><title>Prowlarr</title><item><title>Piratas do Vale do Silicio 1080p</title></item></channel></rss>`

	var receivedQuery url.Values
	var receivedPath string
	var receivedHost string

	// Upstream mock Prowlarr server
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		receivedQuery = r.URL.Query()
		receivedHost = r.Host

		w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
		w.Header().Set("X-Custom-Prowlarr-Header", "torznab-feed")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(sampleXML))
	}))
	defer upstreamServer.Close()

	upstreamURL, err := url.Parse(upstreamServer.URL)
	if err != nil {
		t.Fatalf("failed to parse upstream URL: %v", err)
	}

	fetcher := &mockTitleFetcher{
		titles: map[string]string{
			"12345": "Piratas do Vale do Silício",
		},
	}

	proxyHandler := NewProxy(upstreamURL, fetcher, nil)
	proxyServer := httptest.NewServer(proxyHandler)
	defer proxyServer.Close()

	// Radarr sends request with English query and tmdbid
	reqURL := fmt.Sprintf("%s/api/v1/search?q=Pirates+of+Silicon+Valley&tmdbid=12345&apikey=my-prowlarr-key", proxyServer.URL)
	resp, err := http.Get(reqURL)
	if err != nil {
		t.Fatalf("request through proxy failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read response body: %v", err)
	}

	// Verify exact XML response returned to Radarr
	if !strings.Contains(string(body), "Piratas do Vale do Silicio 1080p") {
		t.Errorf("expected XML body to contain item, got %q", string(body))
	}
	if resp.Header.Get("Content-Type") != "application/rss+xml; charset=utf-8" {
		t.Errorf("unexpected Content-Type header: %s", resp.Header.Get("Content-Type"))
	}

	// Verify upstream received the translated query
	if receivedPath != "/api/v1/search" {
		t.Errorf("expected path '/api/v1/search', got '%s'", receivedPath)
	}
	if receivedQuery.Get("q") != "Piratas do Vale do Silício" {
		t.Errorf("expected 'q' to be translated to 'Piratas do Vale do Silício', got '%s'", receivedQuery.Get("q"))
	}
	if receivedQuery.Get("tmdbid") != "12345" {
		t.Errorf("expected tmdbid '12345', got '%s'", receivedQuery.Get("tmdbid"))
	}
	if receivedQuery.Get("apikey") != "my-prowlarr-key" {
		t.Errorf("expected apikey preserved, got '%s'", receivedQuery.Get("apikey"))
	}
	if receivedHost != upstreamURL.Host {
		t.Errorf("expected Host header %s, got %s", upstreamURL.Host, receivedHost)
	}
}

func TestProxy_PassthroughWithoutTMDBID(t *testing.T) {
	var receivedQuery url.Values

	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedQuery = r.URL.Query()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<caps></caps>"))
	}))
	defer upstreamServer.Close()

	upstreamURL, _ := url.Parse(upstreamServer.URL)
	proxyHandler := NewProxy(upstreamURL, nil, nil)
	proxyServer := httptest.NewServer(proxyHandler)
	defer proxyServer.Close()

	// Torznab caps query (no tmdbid)
	reqURL := fmt.Sprintf("%s/1/api?t=caps&apikey=key123", proxyServer.URL)
	resp, err := http.Get(reqURL)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
	if receivedQuery.Get("t") != "caps" {
		t.Errorf("expected t=caps, got %s", receivedQuery.Get("t"))
	}
	if receivedQuery.Get("apikey") != "key123" {
		t.Errorf("expected apikey=key123, got %s", receivedQuery.Get("apikey"))
	}
}

func TestProxy_GracefulFallbackOnTMDBError(t *testing.T) {
	var upstreamCalled bool

	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled = true
		w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`<rss version="2.0"><channel><title>Prowlarr</title></channel></rss>`))
	}))
	defer upstreamServer.Close()

	upstreamURL, _ := url.Parse(upstreamServer.URL)
	fetcher := &mockTitleFetcher{
		err: errors.New("TMDB rate limit or network error"),
	}

	proxyHandler := NewProxy(upstreamURL, fetcher, nil)
	proxyServer := httptest.NewServer(proxyHandler)
	defer proxyServer.Close()

	// When TMDB fails to find Portuguese titles, the proxy strictly suppresses English queries
	// and returns an empty 200 OK Torznab feed to prevent polluting Radarr with English results.
	reqURL := fmt.Sprintf("%s/api?q=Original+English+Title&tmdbid=99999", proxyServer.URL)
	resp, err := http.Get(reqURL)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
	if upstreamCalled {
		t.Errorf("expected upstream Prowlarr NOT to be called when English fallback is disabled")
	}
}

func TestProxy_UpstreamUnreachable(t *testing.T) {
	// Point to non-listening port
	badURL, _ := url.Parse("http://127.0.0.1:54321")
	proxyHandler := NewProxy(badURL, nil, nil)
	proxyServer := httptest.NewServer(proxyHandler)
	defer proxyServer.Close()

	resp, err := http.Get(proxyServer.URL + "/api/search")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("expected HTTP 502 Bad Gateway when upstream is down, got %d", resp.StatusCode)
	}
}

func TestProxy_Healthz(t *testing.T) {
	upstreamURL, _ := url.Parse("http://localhost:9696")
	proxyHandler := NewProxy(upstreamURL, nil, nil)
	proxyServer := httptest.NewServer(proxyHandler)
	defer proxyServer.Close()

	resp, err := http.Get(proxyServer.URL + "/healthz")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 OK, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "OK" {
		t.Errorf("expected body 'OK', got '%s'", string(body))
	}
}

func TestProxy_PublicTrackerTextSearchTranslation(t *testing.T) {
	var receivedQuery url.Values

	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedQuery = r.URL.Query()
		w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<rss></rss>"))
	}))
	defer upstreamServer.Close()

	upstreamURL, _ := url.Parse(upstreamServer.URL)
	fetcher := &mockTitleFetcher{
		queries: map[string]string{
			"Pirates of Silicon Valley 1999": "Piratas do Vale do Silício 1999",
		},
	}

	proxyHandler := NewProxy(upstreamURL, fetcher, nil)
	proxyServer := httptest.NewServer(proxyHandler)
	defer proxyServer.Close()

	// Intercepted Radarr public tracker search:
	// /1/api?t=search&cat=2000&extended=1&apikey=CHAVE&offset=0&limit=100&q=Pirates%20of%20Silicon%20Valley%201999
	reqURL := fmt.Sprintf("%s/1/api?t=search&cat=2000&extended=1&apikey=CHAVE&offset=0&limit=100&q=Pirates%%20of%%20Silicon%%20Valley%%201999", proxyServer.URL)
	resp, err := http.Get(reqURL)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 OK, got %d", resp.StatusCode)
	}
	if receivedQuery.Get("q") != "Piratas do Vale do Silício 1999" {
		t.Errorf("expected translated query 'Piratas do Vale do Silício 1999', got %q", receivedQuery.Get("q"))
	}
	if receivedQuery.Get("t") != "search" {
		t.Errorf("expected t=search preserved, got %q", receivedQuery.Get("t"))
	}
	if receivedQuery.Get("apikey") != "CHAVE" {
		t.Errorf("expected apikey preserved, got %q", receivedQuery.Get("apikey"))
	}
}

func TestProxy_FanOutAndMergeDeduplication(t *testing.T) {
	// Upstream Prowlarr returns different results depending on the query 'q'
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query().Get("q")
		w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
		w.WriteHeader(http.StatusOK)

		switch query {
		case "Piratas da Informática":
			_, _ = w.Write([]byte(`<?xml version="1.0" encoding="utf-8"?><rss version="2.0"><channel><title>Prowlarr</title>
				<item><title>Piratas da Informatica 1080p</title><guid>guid-1</guid><link>http://t.co/1</link></item>
			</channel></rss>`))
		case "Piratas do Vale do Silício":
			// Returns guid-2 and a duplicate guid-1
			_, _ = w.Write([]byte(`<?xml version="1.0" encoding="utf-8"?><rss version="2.0"><channel><title>Prowlarr</title>
				<item><title>Piratas do Vale do Silicio 720p</title><guid>guid-2</guid><link>http://t.co/2</link></item>
				<item><title>Piratas da Informatica 1080p (dup)</title><guid>guid-1</guid><link>http://t.co/1</link></item>
			</channel></rss>`))
		default:
			_, _ = w.Write([]byte(`<?xml version="1.0" encoding="utf-8"?><rss version="2.0"><channel><title>Prowlarr</title></channel></rss>`))
		}
	}))
	defer upstreamServer.Close()

	upstreamURL, _ := url.Parse(upstreamServer.URL)
	fetcher := &mockTitleFetcher{
		candidates: map[string][]string{
			"Pirates of Silicon Valley 1999": {
				"Piratas da Informática",
				"Piratas do Vale do Silício",
			},
		},
	}

	proxyHandler := NewProxy(upstreamURL, fetcher, nil)
	proxyServer := httptest.NewServer(proxyHandler)
	defer proxyServer.Close()

	reqURL := fmt.Sprintf("%s/1/api?t=search&q=Pirates%%20of%%20Silicon%%20Valley%%201999", proxyServer.URL)
	resp, err := http.Get(reqURL)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	var feed TorznabFeed
	if err := xml.Unmarshal(body, &feed); err != nil {
		t.Fatalf("failed to unmarshal merged Torznab XML: %v", err)
	}

	// Should contain exactly 2 items: guid-1, guid-2 (the duplicate guid-1 is removed)
	if len(feed.Channel.Items) != 2 {
		t.Errorf("expected 2 merged items after deduplication, got %d", len(feed.Channel.Items))
	}

	guids := make(map[string]bool)
	for _, item := range feed.Channel.Items {
		guids[item.Guid] = true
	}
	if !guids["guid-1"] || !guids["guid-2"] {
		t.Errorf("missing expected guids in merged feed: %v", guids)
	}
}

func TestProxy_SonarrTVSearchFanOut(t *testing.T) {
	var requestedQueries []string
	var mu sync.Mutex

	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requestedQueries = append(requestedQueries, r.URL.Query().Get("q"))
		mu.Unlock()

		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprintf(w, `<?xml version="1.0" encoding="utf-8"?><rss version="2.0"><channel><title>Prowlarr</title><item><title>%s 720p</title><guid>%s</guid></item></channel></rss>`, r.URL.Query().Get("q"), r.URL.Query().Get("q"))
	}))
	defer upstreamServer.Close()

	upstreamURL, _ := url.Parse(upstreamServer.URL)
	fetcher := &mockTitleFetcher{
		candidates: map[string][]string{
			"Breaking Bad S01E01": {
				"Breaking Bad S01E01",
				"Ruptura Total S01E01",
			},
		},
	}

	proxyHandler := NewProxy(upstreamURL, fetcher, nil)
	proxyServer := httptest.NewServer(proxyHandler)
	defer proxyServer.Close()

	// Simulate Sonarr query with category 5030,5040 (TV)
	reqURL := fmt.Sprintf("%s/1/api?t=tvsearch&cat=5030,5040&q=Breaking%%20Bad%%20S01E01", proxyServer.URL)
	resp, err := http.Get(reqURL)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(requestedQueries) != 2 {
		t.Errorf("expected 2 fan-out queries for TV show, got %d: %v", len(requestedQueries), requestedQueries)
	}
}

func TestProxy_BypassEmptyQueryRSSSync(t *testing.T) {
	var upstreamCalled bool

	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled = true
		w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="utf-8"?><rss version="2.0"><channel><title>Prowlarr RSS</title><item><title>Latest.Release.1080p</title></item></channel></rss>`))
	}))
	defer upstreamServer.Close()

	upstreamURL, _ := url.Parse(upstreamServer.URL)
	// Fetcher will return error if called, verifying that TMDB is bypassed
	fetcher := &mockTitleFetcher{
		err: errors.New("TMDB should NOT be called on empty query RSS sync"),
	}

	proxyHandler := NewProxy(upstreamURL, fetcher, nil)
	proxyServer := httptest.NewServer(proxyHandler)
	defer proxyServer.Close()

	testCases := []struct {
		name string
		url  string
	}{
		{
			name: "Sonarr RSS Sync (t=tvsearch with empty q)",
			url:  fmt.Sprintf("%s/1/api?t=tvsearch&cat=5030,5040&q=&apikey=my-key", proxyServer.URL),
		},
		{
			name: "Sonarr RSS Sync (t=tvsearch without q param)",
			url:  fmt.Sprintf("%s/1/api?t=tvsearch&cat=5030,5040&apikey=my-key", proxyServer.URL),
		},
		{
			name: "Radarr RSS Sync (t=movie without q param)",
			url:  fmt.Sprintf("%s/1/api?t=movie&cat=2000&apikey=my-key", proxyServer.URL),
		},
		{
			name: "General RSS Sync (t=search with q whitespace)",
			url:  fmt.Sprintf("%s/1/api?t=search&cat=2000&q=%%20%%20&apikey=my-key", proxyServer.URL),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			upstreamCalled = false
			resp, err := http.Get(tc.url)
			if err != nil {
				t.Fatalf("request failed: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				t.Fatalf("expected 200 OK, got HTTP %d", resp.StatusCode)
			}
			if !upstreamCalled {
				t.Errorf("expected request to be forwarded directly to Upstream Prowlarr")
			}

			body, _ := io.ReadAll(resp.Body)
			if !strings.Contains(string(body), "Latest.Release.1080p") {
				t.Errorf("expected Upstream RSS feed content, got %s", string(body))
			}
		})
	}
}
