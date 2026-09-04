// Command reproxy runs the retry reverse proxy server: it parses CLI flags
// into a ServerConfig, wires the proxy handler, and serves until SIGINT or
// SIGTERM triggers a graceful drain.
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
	"strings"
	"syscall"
	"time"

	"reproxy"
)

// readHeaderTimeout bounds how long the server waits for request headers
// (a slowloris guard; a general-purpose hardening default).
const readHeaderTimeout = 10 * time.Second

// shutdownDrain is how long graceful shutdown waits for in-flight requests
// after SIGINT/SIGTERM before returning.
const shutdownDrain = 10 * time.Second

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)
	log.SetPrefix("reproxy: ")

	fs := flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
	cfg, err := reproxy.ParseFlags(fs, os.Args[1:])
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		fmt.Fprintf(os.Stderr, "reproxy: %v\n\n", err)
		fmt.Fprintf(os.Stderr, "usage: reproxy --listen %s --allowlist example.com\nsee \"reproxy -h\" for all flags\n", reproxy.DefaultListen)
		os.Exit(2)
	}

	proxy := reproxy.NewProxy(cfg)
	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           proxy,
		ReadHeaderTimeout: readHeaderTimeout,
	}

	logStartup(cfg)

	// Serve until a termination signal or a fatal ListenAndServe error.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("FATAL: server failed: %v", err)
			os.Exit(1)
		}
	case <-ctx.Done():
		log.Printf("event=shutdown signal received; draining in-flight requests (up to %s)", shutdownDrain)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownDrain)
		defer cancel()
		if serr := srv.Shutdown(shutdownCtx); serr != nil {
			log.Printf("event=shutdown graceful drain failed: %v", serr)
			os.Exit(1)
		}
	}
	log.Printf("event=shutdown complete")
}

// logStartup emits the startup summary: the listen address, the destination
// gate in force (allowlist entries or the dangerous allow-all escape hatch),
// and the server caps. The dangerous-allow-all warning is its own line so it
// cannot be missed in logs.
func logStartup(cfg *reproxy.ServerConfig) {
	log.Printf("event=startup listen=%s max_attempts=%d max_budget=%s max_body=%d strict_body_limit=%t",
		cfg.Listen, cfg.MaxAttempts, cfg.MaxBudget, cfg.MaxBody, cfg.StrictBodyLimit)

	if cfg.DangerousAllowAll {
		log.Printf("WARNING: --dangerous-allow-all is set: this proxy forwards to ANY public host (open relay exposure)")
		log.Printf("event=startup destination gate=disabled (--dangerous-allow-all)")
		return
	}
	log.Printf("event=startup destination allowlist=[%s]", strings.Join(cfg.Allowlist, ","))
}
