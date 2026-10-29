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
