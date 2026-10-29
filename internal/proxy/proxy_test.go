package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

type mockTitleFetcher struct {
	titles map[string]string
	err    error
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
	if string(body) != sampleXML {
		t.Errorf("expected XML body %q, got %q", sampleXML, string(body))
	}
	if resp.Header.Get("Content-Type") != "application/rss+xml; charset=utf-8" {
		t.Errorf("unexpected Content-Type header: %s", resp.Header.Get("Content-Type"))
	}
	if resp.Header.Get("X-Custom-Prowlarr-Header") != "torznab-feed" {
		t.Errorf("unexpected custom header: %s", resp.Header.Get("X-Custom-Prowlarr-Header"))
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
	var receivedQuery url.Values

	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedQuery = r.URL.Query()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<fallback></fallback>"))
	}))
	defer upstreamServer.Close()

	upstreamURL, _ := url.Parse(upstreamServer.URL)
	fetcher := &mockTitleFetcher{
		err: errors.New("TMDB rate limit or network error"),
	}

	proxyHandler := NewProxy(upstreamURL, fetcher, nil)
	proxyServer := httptest.NewServer(proxyHandler)
	defer proxyServer.Close()

	// Should fallback and keep original q
	reqURL := fmt.Sprintf("%s/api?q=Original+English+Title&tmdbid=99999", proxyServer.URL)
	resp, err := http.Get(reqURL)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
	if receivedQuery.Get("q") != "Original English Title" {
		t.Errorf("expected fallback to 'Original English Title', got '%s'", receivedQuery.Get("q"))
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
