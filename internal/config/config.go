package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds the application configuration loaded from environment variables.
type Config struct {
	// Port is the HTTP port the proxy listens on (default: "9697").
	Port string

	// Host is the network interface to bind to (default: "0.0.0.0").
	Host string

	// ProwlarrURL is the parsed upstream URL of the Prowlarr server (default: "http://localhost:9696").
	ProwlarrURL *url.URL

	// TMDBApiKey is the API key for The Movie Database (required for translations).
	TMDBApiKey string

	// TMDBBaseURL is the base URL for the TMDB API (default: "https://api.themoviedb.org/3").
	TMDBBaseURL string

	// CacheTTL is how long TMDB translated titles remain cached in memory (default: 24h).
	CacheTTL time.Duration

	// RequestTimeout is the timeout for outgoing HTTP calls to TMDB and Prowlarr (default: 15s).
	RequestTimeout time.Duration
}

// Address returns the combined host:port string for http.Server.
func (c *Config) Address() string {
	return fmt.Sprintf("%s:%s", c.Host, c.Port)
}

// Load loads and validates configuration from environment variables.
func Load() (*Config, error) {
	port := getEnv("PORT", "9697")
	host := getEnv("HOST", "0.0.0.0")

	// Validate port number
	if portNum, err := strconv.Atoi(port); err != nil || portNum < 1 || portNum > 65535 {
		return nil, fmt.Errorf("invalid PORT '%s': must be a valid port number between 1 and 65535", port)
	}

	rawProwlarrURL := getEnv("PROWLARR_URL", "http://localhost:9696")
	prowlarrURL, err := url.Parse(rawProwlarrURL)
	if err != nil || prowlarrURL.Scheme == "" || prowlarrURL.Host == "" {
		return nil, fmt.Errorf("invalid PROWLARR_URL '%s': must be a valid absolute URL (e.g. http://localhost:9696)", rawProwlarrURL)
	}

	tmdbApiKey := os.Getenv("TMDB_API_KEY")
	tmdbBaseURL := getEnv("TMDB_BASE_URL", "https://api.themoviedb.org/3")
	// Trim trailing slash for consistent URL building
	tmdbBaseURL = strings.TrimRight(tmdbBaseURL, "/")

	cacheTTLMinutesStr := getEnv("CACHE_TTL_MINUTES", "1440") // 24 hours
	cacheTTLMinutes, err := strconv.Atoi(cacheTTLMinutesStr)
	if err != nil || cacheTTLMinutes < 1 {
		cacheTTLMinutes = 1440
	}

	timeoutSecStr := getEnv("REQUEST_TIMEOUT_SECONDS", "15")
	timeoutSec, err := strconv.Atoi(timeoutSecStr)
	if err != nil || timeoutSec < 1 {
		timeoutSec = 15
	}

	cfg := &Config{
		Port:           port,
		Host:           host,
		ProwlarrURL:    prowlarrURL,
		TMDBApiKey:     tmdbApiKey,
		TMDBBaseURL:    tmdbBaseURL,
		CacheTTL:       time.Duration(cacheTTLMinutes) * time.Minute,
		RequestTimeout: time.Duration(timeoutSec) * time.Second,
	}

	return cfg, nil
}

// Validate checks if critical configuration items are present.
// Note: TMDBApiKey is validated separately if strict mode is desired,
// but the proxy can still run in passthrough mode even without TMDB_API_KEY.
func (c *Config) Validate() error {
	if c.TMDBApiKey == "" {
		return errors.New("TMDB_API_KEY is not set: title translation will be disabled and requests passed through as-is")
	}
	return nil
}

// getEnv returns the value of an environment variable or a fallback default.
func getEnv(key, fallback string) string {
	if val, ok := os.LookupEnv(key); ok && strings.TrimSpace(val) != "" {
		return strings.TrimSpace(val)
	}
	return fallback
}
