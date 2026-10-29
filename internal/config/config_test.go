package config

import (
	"os"
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	os.Clearenv()

	cfg, err := Load()
	if err != nil {
		t.Fatalf("expected no error loading defaults, got: %v", err)
	}

	if cfg.Port != "9697" {
		t.Errorf("expected default Port '9697', got '%s'", cfg.Port)
	}
	if cfg.Host != "0.0.0.0" {
		t.Errorf("expected default Host '0.0.0.0', got '%s'", cfg.Host)
	}
	if cfg.Address() != "0.0.0.0:9697" {
		t.Errorf("expected Address '0.0.0.0:9697', got '%s'", cfg.Address())
	}
	if cfg.ProwlarrURL.String() != "http://localhost:9696" {
		t.Errorf("expected default ProwlarrURL 'http://localhost:9696', got '%s'", cfg.ProwlarrURL.String())
	}
	if cfg.TMDBBaseURL != "https://api.themoviedb.org/3" {
		t.Errorf("expected default TMDBBaseURL 'https://api.themoviedb.org/3', got '%s'", cfg.TMDBBaseURL)
	}
	if cfg.CacheTTL != 24*time.Hour {
		t.Errorf("expected default CacheTTL 24h, got %v", cfg.CacheTTL)
	}
	if cfg.RequestTimeout != 15*time.Second {
		t.Errorf("expected default RequestTimeout 15s, got %v", cfg.RequestTimeout)
	}
	if cfg.TMDBApiKey != "" {
		t.Errorf("expected empty TMDBApiKey, got '%s'", cfg.TMDBApiKey)
	}
}

func TestLoadCustomValues(t *testing.T) {
	os.Clearenv()
	os.Setenv("PORT", "8080")
	os.Setenv("HOST", "127.0.0.1")
	os.Setenv("PROWLARR_URL", "http://192.168.1.50:9696")
	os.Setenv("TMDB_API_KEY", "test_tmdb_key_123")
	os.Setenv("TMDB_BASE_URL", "https://api.example.com/tmdb/")
	os.Setenv("CACHE_TTL_MINUTES", "120")
	os.Setenv("REQUEST_TIMEOUT_SECONDS", "30")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Port != "8080" {
		t.Errorf("expected Port '8080', got '%s'", cfg.Port)
	}
	if cfg.Address() != "127.0.0.1:8080" {
		t.Errorf("expected Address '127.0.0.1:8080', got '%s'", cfg.Address())
	}
	if cfg.ProwlarrURL.String() != "http://192.168.1.50:9696" {
		t.Errorf("expected ProwlarrURL 'http://192.168.1.50:9696', got '%s'", cfg.ProwlarrURL.String())
	}
	if cfg.TMDBApiKey != "test_tmdb_key_123" {
		t.Errorf("expected TMDBApiKey 'test_tmdb_key_123', got '%s'", cfg.TMDBApiKey)
	}
	if cfg.TMDBBaseURL != "https://api.example.com/tmdb" {
		t.Errorf("expected TMDBBaseURL trailing slash stripped 'https://api.example.com/tmdb', got '%s'", cfg.TMDBBaseURL)
	}
	if cfg.CacheTTL != 2*time.Hour {
		t.Errorf("expected CacheTTL 2h, got %v", cfg.CacheTTL)
	}
	if cfg.RequestTimeout != 30*time.Second {
		t.Errorf("expected RequestTimeout 30s, got %v", cfg.RequestTimeout)
	}
}

func TestLoadInvalidValues(t *testing.T) {
	// Invalid port
	os.Clearenv()
	os.Setenv("PORT", "invalid")
	if _, err := Load(); err == nil {
		t.Errorf("expected error for invalid PORT, got nil")
	}

	// Port out of range
	os.Clearenv()
	os.Setenv("PORT", "70000")
	if _, err := Load(); err == nil {
		t.Errorf("expected error for out-of-range PORT, got nil")
	}

	// Invalid PROWLARR_URL
	os.Clearenv()
	os.Setenv("PROWLARR_URL", "not-a-valid-url")
	if _, err := Load(); err == nil {
		t.Errorf("expected error for invalid PROWLARR_URL without scheme, got nil")
	}
}
