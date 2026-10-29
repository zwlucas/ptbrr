package proxy

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

// TitleFetcher is an abstraction for querying localized movie titles.
type TitleFetcher interface {
	GetMovieTitle(ctx context.Context, tmdbID string) (string, error)
}

// Proxy is a Torznab reverse proxy middleware that translates English movie search
// queries into Brazilian Portuguese (pt-BR) using the TMDB API before forwarding to Prowlarr.
type Proxy struct {
	upstreamURL  *url.URL
	titleFetcher TitleFetcher
	reverseProxy *httputil.ReverseProxy
	logger       *log.Logger
}

// responseRecorder captures the status code sent to the client for structured logging.
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
	}

	// Initialize standard library reverse proxy
	revProxy := httputil.NewSingleHostReverseProxy(upstreamURL)

	// Custom Director: adjusts upstream scheme, host, and headers
	originalDirector := revProxy.Director
	revProxy.Director = func(req *http.Request) {
		originalDirector(req)
		// Ensure upstream Host matches the target host
		req.Host = upstreamURL.Host

		// Set forwarding headers
		req.Header.Set("X-Forwarded-Host", req.Host)
		req.Header.Set("X-Forwarded-Proto", upstreamURL.Scheme)
	}

	// Custom ErrorHandler to return 502 Bad Gateway if Prowlarr is unreachable
	revProxy.ErrorHandler = func(w http.ResponseWriter, req *http.Request, err error) {
		p.logger.Printf("[PROXY ERROR] Failed to connect to upstream Prowlarr (%s): %v", upstreamURL.String(), err)
		http.Error(w, fmt.Sprintf("502 Bad Gateway: Upstream Prowlarr (%s) is unreachable: %v", upstreamURL.String(), err), http.StatusBadGateway)
	}

	p.reverseProxy = revProxy
	return p
}

// ServeHTTP handles incoming requests, intercepts tmdbid, modifies query, and proxies to upstream.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	// 1. Health check endpoint for monitoring / LXC liveness
	if r.URL.Path == "/healthz" || r.URL.Path == "/ping" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
		return
	}

	// 2. Parse query parameters
	queryParams := r.URL.Query()
	tmdbID := strings.TrimSpace(queryParams.Get("tmdbid"))

	// 3. Intercept tmdbid if present
	if tmdbID != "" && p.titleFetcher != nil {
		originalQuery := queryParams.Get("q")

		translatedTitle, err := p.titleFetcher.GetMovieTitle(r.Context(), tmdbID)
		if err != nil {
			p.logger.Printf("[TMDB WARNING] tmdbid=%s lookup failed: %v (keeping original q=%q)", tmdbID, err, originalQuery)
		} else if translatedTitle != "" {
			// Replace or set the 'q' parameter with pt-BR title
			queryParams.Set("q", translatedTitle)
			r.URL.RawQuery = queryParams.Encode()

			p.logger.Printf("[TMDB TRANSLATE] tmdbid=%s: %q -> %q", tmdbID, originalQuery, translatedTitle)
		}
	}

	// 4. Wrap response writer to capture status code for logging
	recorder := &responseRecorder{
		ResponseWriter: w,
		statusCode:     http.StatusOK, // Default if WriteHeader is not explicitly called
	}

	// 5. Forward to upstream Prowlarr and pipe response back to Radarr
	p.reverseProxy.ServeHTTP(recorder, r)

	p.logger.Printf("[PROXY] %s %s -> status=%d duration=%v", r.Method, r.URL.RequestURI(), recorder.statusCode, time.Since(start))
}
