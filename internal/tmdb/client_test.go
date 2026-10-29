package tmdb

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestGetMovieTitle_Success(t *testing.T) {
	var requestCount int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requestCount, 1)

		// Verify request path and query parameters
		if r.URL.Path != "/movie/12345" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.URL.Query().Get("api_key") != "secret-tmdb-key" {
			t.Errorf("unexpected api_key: %s", r.URL.Query().Get("api_key"))
		}
		if r.URL.Query().Get("language") != "pt-BR" {
			t.Errorf("unexpected language: %s", r.URL.Query().Get("language"))
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"id":             12345,
			"title":          "Piratas do Vale do Silício",
			"original_title": "Pirates of Silicon Valley",
		})
	}))
	defer server.Close()

	client := NewClient("secret-tmdb-key", server.URL, 5*time.Second, 1*time.Hour)

	ctx := context.Background()

	// 1st request: should hit mock server
	title, err := client.GetMovieTitle(ctx, "12345")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if title != "Piratas do Vale do Silício" {
		t.Errorf("expected 'Piratas do Vale do Silício', got '%s'", title)
	}
	if atomic.LoadInt32(&requestCount) != 1 {
		t.Errorf("expected 1 HTTP request, got %d", atomic.LoadInt32(&requestCount))
	}

	// 2nd request: should hit in-memory cache, no HTTP call
	cachedTitle, err := client.GetMovieTitle(ctx, "12345")
	if err != nil {
		t.Fatalf("unexpected error on cached call: %v", err)
	}
	if cachedTitle != "Piratas do Vale do Silício" {
		t.Errorf("expected cached 'Piratas do Vale do Silício', got '%s'", cachedTitle)
	}
	if atomic.LoadInt32(&requestCount) != 1 {
		t.Errorf("expected still 1 HTTP request (cache hit), got %d", atomic.LoadInt32(&requestCount))
	}
}

func TestGetMovieTitle_NotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"status_code":34,"status_message":"The resource you requested could not be found."}`))
	}))
	defer server.Close()

	client := NewClient("secret-tmdb-key", server.URL, 5*time.Second, 1*time.Hour)

	_, err := client.GetMovieTitle(context.Background(), "99999999")
	if err == nil {
		t.Fatal("expected error for 404, got nil")
	}
}

func TestGetMovieTitle_EmptyTitle(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"id":    12345,
			"title": "   ",
		})
	}))
	defer server.Close()

	client := NewClient("secret-tmdb-key", server.URL, 5*time.Second, 1*time.Hour)

	_, err := client.GetMovieTitle(context.Background(), "12345")
	if err != ErrTitleNotFound {
		t.Fatalf("expected ErrTitleNotFound, got: %v", err)
	}
}

func TestGetMovieTitle_MissingAPIKey(t *testing.T) {
	client := NewClient("", "https://api.themoviedb.org/3", 5*time.Second, 1*time.Hour)
	_, err := client.GetMovieTitle(context.Background(), "12345")
	if err != ErrMissingAPIKey {
		t.Fatalf("expected ErrMissingAPIKey, got: %v", err)
	}
}

func TestGetMovieTitle_EmptyTMDBID(t *testing.T) {
	client := NewClient("key", "https://api.themoviedb.org/3", 5*time.Second, 1*time.Hour)
	_, err := client.GetMovieTitle(context.Background(), "   ")
	if err != ErrEmptyTMDBID {
		t.Fatalf("expected ErrEmptyTMDBID, got: %v", err)
	}
}

func TestCacheExpiration(t *testing.T) {
	var count int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&count, 1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"id":    1,
			"title": "Matrix",
		})
	}))
	defer server.Close()

	// Short TTL of 50ms
	client := NewClient("key", server.URL, 5*time.Second, 50*time.Millisecond)

	_, err := client.GetMovieTitle(context.Background(), "1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if atomic.LoadInt32(&count) != 1 {
		t.Errorf("expected 1 call, got %d", atomic.LoadInt32(&count))
	}

	// Sleep past TTL
	time.Sleep(70 * time.Millisecond)

	_, err = client.GetMovieTitle(context.Background(), "1")
	if err != nil {
		t.Fatalf("unexpected error after expiry: %v", err)
	}
	if atomic.LoadInt32(&count) != 2 {
		t.Errorf("expected 2 calls after expiry, got %d", atomic.LoadInt32(&count))
	}
}

func TestPurgeExpired(t *testing.T) {
	client := NewClient("key", "http://example.com", 5*time.Second, 50*time.Millisecond)
	client.setInCache("1", "Title 1")
	client.setInCache("2", "Title 2")

	if client.CacheCount() != 2 {
		t.Fatalf("expected cache count 2, got %d", client.CacheCount())
	}

	time.Sleep(60 * time.Millisecond)
	purged := client.PurgeExpired()
	if purged != 2 {
		t.Errorf("expected 2 purged entries, got %d", purged)
	}
	if client.CacheCount() != 0 {
		t.Errorf("expected cache count 0, got %d", client.CacheCount())
	}
}

func TestTranslateByQuery_Success(t *testing.T) {
	var searchCalls int32
	var detailCalls int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if r.URL.Path == "/search/movie" {
			atomic.AddInt32(&searchCalls, 1)
			if r.URL.Query().Get("query") != "Pirates of Silicon Valley" {
				t.Errorf("expected clean query 'Pirates of Silicon Valley', got %q", r.URL.Query().Get("query"))
			}
			if r.URL.Query().Get("year") != "1999" {
				t.Errorf("expected year 1999, got %q", r.URL.Query().Get("year"))
			}

			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"results": []map[string]interface{}{
					{"id": 12345, "title": "Pirates of Silicon Valley"},
				},
			})
			return
		}

		if r.URL.Path == "/movie/12345" {
			atomic.AddInt32(&detailCalls, 1)
			if r.URL.Query().Get("language") != "pt-BR" {
				t.Errorf("expected language pt-BR, got %s", r.URL.Query().Get("language"))
			}

			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"id":    12345,
				"title": "Piratas do Vale do Silício",
			})
			return
		}

		http.NotFound(w, r)
	}))
	defer server.Close()

	client := NewClient("test-key", server.URL, 5*time.Second, 1*time.Hour)

	// First query
	res, err := client.TranslateByQuery(context.Background(), "Pirates of Silicon Valley 1999")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res != "Piratas do Vale do Silício 1999" {
		t.Errorf("expected 'Piratas do Vale do Silício 1999', got %q", res)
	}
	if atomic.LoadInt32(&searchCalls) != 1 || atomic.LoadInt32(&detailCalls) != 1 {
		t.Errorf("expected 1 search and 1 detail call, got %d and %d", searchCalls, detailCalls)
	}

	// Second query should hit cache
	res2, err := client.TranslateByQuery(context.Background(), "Pirates of Silicon Valley 1999")
	if err != nil {
		t.Fatalf("unexpected error on cached query: %v", err)
	}
	if res2 != "Piratas do Vale do Silício 1999" {
		t.Errorf("expected cached result 'Piratas do Vale do Silício 1999', got %q", res2)
	}
	if atomic.LoadInt32(&searchCalls) != 1 || atomic.LoadInt32(&detailCalls) != 1 {
		t.Errorf("cache failed, searchCalls=%d, detailCalls=%d", searchCalls, detailCalls)
	}
}

func TestExtractYearAndCleanTitle(t *testing.T) {
	tests := []struct {
		input      string
		expectedC  string
		expectedYr string
	}{
		{"Pirates of Silicon Valley 1999", "Pirates of Silicon Valley", "1999"},
		{"The Matrix (1999)", "The Matrix", "1999"},
		{"The Dark Knight", "The Dark Knight", ""},
		{"2012", "2012", ""},
		{"Blade Runner 2049", "Blade Runner", "2049"},
	}

	for _, tt := range tests {
		clean, yr := extractYearAndCleanTitle(tt.input)
		if clean != tt.expectedC || yr != tt.expectedYr {
			t.Errorf("extractYearAndCleanTitle(%q) = (%q, %q), want (%q, %q)", tt.input, clean, yr, tt.expectedC, tt.expectedYr)
		}
	}
}

func TestGetAlternativeTitles_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/movie/12345/alternative_titles" {
			http.NotFound(w, r)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"id": 12345,
			"titles": []map[string]interface{}{
				{"iso_3166_1": "BR", "title": "Piratas do Vale do Silício"},
				{"iso_3166_1": "PT", "title": "Piratas de Silicon Valley"},
				{"iso_3166_1": "ES", "title": "Piratas de Silicon Valley (ES)"},
			},
		})
	}))
	defer server.Close()

	client := NewClient("test-key", server.URL, 5*time.Second, 1*time.Hour)
	alts, err := client.GetAlternativeTitles(context.Background(), "12345")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should only have BR and PT (2 titles)
	if len(alts) != 2 {
		t.Fatalf("expected 2 alternative titles, got %d: %v", len(alts), alts)
	}
	if alts[0] != "Piratas do Vale do Silício" || alts[1] != "Piratas de Silicon Valley" {
		t.Errorf("unexpected titles: %v", alts)
	}
}

func TestGetSearchCandidates(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch r.URL.Path {
		case "/search/movie":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"results": []map[string]interface{}{
					{"id": 12345, "title": "Pirates of Silicon Valley"},
				},
			})
		case "/movie/12345":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"id":    12345,
				"title": "Piratas da Informática",
			})
		case "/movie/12345/alternative_titles":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"id": 12345,
				"titles": []map[string]interface{}{
					{"iso_3166_1": "BR", "title": "Piratas do Vale do Silício"},
					{"iso_3166_1": "US", "title": "Silicon Valley Pirates"},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewClient("test-key", server.URL, 5*time.Second, 1*time.Hour)

	candidates, err := client.GetSearchCandidates(context.Background(), "Pirates of Silicon Valley 1999", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Expected candidates (NO year appended, NO English fallback):
	// 1. "Piratas da Informática" (Official pt-BR)
	// 2. "Piratas do Vale do Silício" (Alternative BR)
	expected := []string{
		"Piratas da Informática",
		"Piratas do Vale do Silício",
	}

	if len(candidates) != len(expected) {
		t.Fatalf("expected %d candidates, got %d: %v", len(expected), len(candidates), candidates)
	}
	for i, exp := range expected {
		if candidates[i] != exp {
			t.Errorf("candidate[%d] = %q, expected %q", i, candidates[i], exp)
		}
	}
}
