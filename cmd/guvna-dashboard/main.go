// guvna-dashboard — optional web UI for a Guvna gateway. Proxies a live
// gateway over HTTP (same endpoints as guvna-cli) and renders server-side
// HTML (Go templates + HTMX). The admin key stays server-side in env;
// browsers only see rendered HTML.
//
// Never exposed directly: bind localhost and put a reverse proxy with
// access control (basic_auth / IP allowlist) in front. Zero gateway
// changes required; run only when wanted:
//
//	GUVNA_URL=http://127.0.0.1:20128 GUVNA_ADMIN_KEY=... guvna-dashboard
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

	"github.com/creamy-ghost/guvna/internal/dashboard"
)

const defaultAddr = "127.0.0.1:20129"

func main() {
	log.SetPrefix("guvna-dashboard: ")
	addr := flag.String("addr", defaultAddr, "listen address (keep localhost)")
	gateway := flag.String("gateway", envOr("GUVNA_URL", "http://127.0.0.1:20128"), "gateway base URL")
	healthCheck := flag.Bool("healthcheck", false, "check /healthz of a running instance and exit 0/1")
	flag.Parse()

	if *healthCheck {
		os.Exit(doHealthCheck(*addr))
	}

	adminKey := os.Getenv("GUVNA_ADMIN_KEY")
	if adminKey == "" {
		// Same key, compose-local name: deploy/.env carries ADMIN_KEY
		// (shared with the gateway service in the same compose file).
		adminKey = os.Getenv("ADMIN_KEY")
	}
	if adminKey == "" {
		log.Fatalf("GUVNA_ADMIN_KEY (or ADMIN_KEY) env var required")
	}

	d, err := dashboard.New(*gateway, adminKey)
	if err != nil {
		log.Fatalf("dashboard: %v", err)
	}
	log.Printf("gateway=%s", *gateway)

	srv := &http.Server{
		Addr:              *addr,
		Handler:           d.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("listening on %s", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Printf("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
}

// doHealthCheck probes a running dashboard's /healthz. Distroless images
// have no shell or curl, so the binary itself is the healthcheck.
func doHealthCheck(addr string) int {
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://%s/healthz", addr))
	if err != nil {
		log.Printf("healthcheck: %v", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Printf("healthcheck: status %d", resp.StatusCode)
		return 1
	}
	return 0
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
