package main_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ptbrr/internal/proxy"
	"ptbrr/internal/tmdb"
)

func TestEndToEndTorznabTranslationFlow(t *testing.T) {
	var tmdbCallCount int32
	var prowlarrCallCount int32
	var queriesMu sync.Mutex
	var prowlarrQueries []string

	// 1. Mock TMDB API Server
	mockTMDB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&tmdbCallCount, 1)

		if r.URL.Path == "/search/movie" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"results": []map[string]interface{}{
					{"id": 603, "title": "The Matrix"},
				},
			})
			return
		}

		if r.URL.Path == "/movie/603/alternative_titles" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"id": 603,
				"titles": []map[string]interface{}{
					{"iso_3166_1": "BR", "title": "Matrix"},
				},
			})
			return
		}

		if r.URL.Path != "/movie/603" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("api_key") != "my-tmdb-key" {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"id":             603,
			"title":          "Matrix",
			"original_title": "The Matrix",
		})
	}))
	defer mockTMDB.Close()

	// 2. Mock Upstream Prowlarr Server
	torznabXML := `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:torznab="http://torznab.com/schemas/2015/feed">
  <channel>
    <title>Prowlarr Indexer</title>
    <item>
      <title>Matrix (1999) 2160p Dual Audio Remux</title>
      <guid>guid-matrix-1</guid>
      <link>http://tracker/1</link>
      <size>45000000000</size>
    </item>
  </channel>
</rss>`

	mockProwlarr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&prowlarrCallCount, 1)
		queriesMu.Lock()
		prowlarrQueries = append(prowlarrQueries, r.URL.Query().Get("q"))
		queriesMu.Unlock()

		w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(torznabXML))
	}))
	defer mockProwlarr.Close()

	prowlarrURL, err := url.Parse(mockProwlarr.URL)
	if err != nil {
		t.Fatalf("failed to parse mock prowlarr url: %v", err)
	}

	// 3. Initialize Proxy with TMDB client
	tmdbClient := tmdb.NewClient("my-tmdb-key", mockTMDB.URL, 5*time.Second, 1*time.Hour)
	proxyHandler := proxy.NewProxy(prowlarrURL, tmdbClient, nil)

	proxyServer := httptest.NewServer(proxyHandler)
	defer proxyServer.Close()

	// 4. Radarr performs Torznab search with English query & tmdbid
	// Fan-Out should search both "Matrix" and "The Matrix"
	radarrURL := fmt.Sprintf("%s/api/v1/search?q=The+Matrix&tmdbid=603&apikey=secret-prowlarr-token", proxyServer.URL)

	resp, err := http.Get(radarrURL)
	if err != nil {
		t.Fatalf("Radarr search failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read body: %v", err)
	}

	// 5. Verify returned XML body contains the item
	if !strings.Contains(string(body), "Matrix (1999) 2160p Dual Audio Remux") {
		t.Errorf("expected XML response to contain item, got %q", string(body))
	}

	// 6. Verify Fan-Out executed Prowlarr search for the Portuguese title (strictly without English fallback)
	if atomic.LoadInt32(&tmdbCallCount) != 2 {
		t.Errorf("expected 2 TMDB calls (details + alternative titles), got %d", atomic.LoadInt32(&tmdbCallCount))
	}
	if atomic.LoadInt32(&prowlarrCallCount) != 1 {
		t.Errorf("expected 1 Prowlarr call for 'Matrix' (no English fallback), got %d", atomic.LoadInt32(&prowlarrCallCount))
	}

	// 7. Verify Cache on 2nd identical request: TMDB should NOT be called again
	resp2, err := http.Get(radarrURL)
	if err != nil {
		t.Fatalf("2nd Radarr search failed: %v", err)
	}
	resp2.Body.Close()

	if atomic.LoadInt32(&tmdbCallCount) != 2 {
		t.Errorf("expected TMDB call count to remain 2 due to cache, got %d", atomic.LoadInt32(&tmdbCallCount))
	}
	if atomic.LoadInt32(&prowlarrCallCount) != 2 {
		t.Errorf("expected Prowlarr call count to be 2, got %d", atomic.LoadInt32(&prowlarrCallCount))
	}

	// 8. Test Torznab caps request (no tmdbid): TMDB should NOT be called
	capsURL := fmt.Sprintf("%s/api?t=caps&apikey=secret-prowlarr-token", proxyServer.URL)
	resp3, err := http.Get(capsURL)
	if err != nil {
		t.Fatalf("Caps request failed: %v", err)
	}
	resp3.Body.Close()

	if atomic.LoadInt32(&tmdbCallCount) != 2 {
		t.Errorf("TMDB should not be called for requests without tmdbid, got %d", atomic.LoadInt32(&tmdbCallCount))
	}
	if atomic.LoadInt32(&prowlarrCallCount) != 3 {
		t.Errorf("expected Prowlarr call count to be 3, got %d", atomic.LoadInt32(&prowlarrCallCount))
	}

	// 9. Public tracker search without tmdbid (Clean title without year, strictly Portuguese):
	// /1/api?t=search&cat=2000&extended=1&apikey=secret-prowlarr-token&offset=0&limit=100&q=The+Matrix+1999
	publicTrackerURL := fmt.Sprintf("%s/1/api?t=search&cat=2000&extended=1&apikey=secret-prowlarr-token&offset=0&limit=100&q=The+Matrix+1999", proxyServer.URL)
	resp4, err := http.Get(publicTrackerURL)
	if err != nil {
		t.Fatalf("Public tracker search failed: %v", err)
	}
	resp4.Body.Close()

	if resp4.StatusCode != http.StatusOK {
		t.Errorf("expected 200 OK, got %d", resp4.StatusCode)
	}

	queriesMu.Lock()
	foundCleanTitle := false
	foundEnglishTitle := false
	for _, q := range prowlarrQueries {
		if q == "Matrix" {
			foundCleanTitle = true
		}
		if q == "The Matrix" || q == "The Matrix 1999" {
			foundEnglishTitle = true
		}
	}
	queriesMu.Unlock()

	if !foundCleanTitle {
		t.Errorf("expected clean Portuguese title 'Matrix' in Prowlarr queries, got %v", prowlarrQueries)
	}
	if foundEnglishTitle {
		t.Errorf("found unwanted English query in Prowlarr queries (fallback should be disabled): %v", prowlarrQueries)
	}
}
