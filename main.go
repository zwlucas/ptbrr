package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"ptbrr/internal/config"
	"ptbrr/internal/proxy"
	"ptbrr/internal/tmdb"
)

// version is injected at build time via -ldflags="-X main.version=vX.Y.Z".
var version = "dev"

func main() {
	showVersion := flag.Bool("version", false, "Print version information and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("ptbrr version %s\n", version)
		os.Exit(0)
	}

	logger := log.New(os.Stdout, "", log.LstdFlags)

	logger.Printf("==================================================")
	logger.Printf("  ptbrr - Torznab pt-BR Translation Reverse Proxy ")
	logger.Printf("  Version: %s", version)
	logger.Printf("==================================================")

	// Load configuration
	cfg, err := config.Load()
	if err != nil {
		logger.Fatalf("[FATAL] Configuration error: %v", err)
	}

	// Validate TMDB API Key
	if err := cfg.Validate(); err != nil {
		logger.Printf("[WARNING] %v", err)
	} else {
		logger.Printf("[CONFIG] TMDB API Key: configured (first 4 chars: %s****)", maskKey(cfg.TMDBApiKey))
	}

	logger.Printf("[CONFIG] Listening Address : %s", cfg.Address())
	logger.Printf("[CONFIG] Upstream Prowlarr : %s", cfg.ProwlarrURL.String())
	logger.Printf("[CONFIG] TMDB Base URL     : %s", cfg.TMDBBaseURL)
	logger.Printf("[CONFIG] Cache TTL         : %v", cfg.CacheTTL)
	logger.Printf("[CONFIG] Request Timeout   : %v", cfg.RequestTimeout)

	// Initialize TMDB client
	var titleFetcher proxy.TitleFetcher
	if cfg.TMDBApiKey != "" {
		titleFetcher = tmdb.NewClient(cfg.TMDBApiKey, cfg.TMDBBaseURL, cfg.RequestTimeout, cfg.CacheTTL)
	}

	// Initialize reverse proxy
	proxyHandler := proxy.NewProxy(cfg.ProwlarrURL, titleFetcher, logger)

	server := &http.Server{
		Addr:              cfg.Address(),
		Handler:           proxyHandler,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// Setup graceful shutdown channel
	shutdownChan := make(chan os.Signal, 1)
	signal.Notify(shutdownChan, os.Interrupt, syscall.SIGTERM)

	// Start HTTP server in background goroutine
	go func() {
		logger.Printf("[SERVER] Starting reverse proxy on http://%s ...", cfg.Address())
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Fatalf("[FATAL] HTTP server failed: %v", err)
		}
	}()

	// Wait for termination signal
	sig := <-shutdownChan
	logger.Printf("[SERVER] Received shutdown signal: %v. Initiating graceful shutdown...", sig)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Fatalf("[FATAL] Graceful shutdown failed: %v", err)
	}

	logger.Printf("[SERVER] Proxy shut down cleanly. Bye!")
}

func maskKey(key string) string {
	if len(key) <= 4 {
		return "****"
	}
	return key[:4]
}
