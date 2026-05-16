package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bryanneva/dingdong/internal/server"
)

func main() {
	addr := flag.String("addr", envOr("DINGDONG_ADDR", ":8080"), "listen address")
	capacity := flag.Int("capacity", 1000, "max in-memory knocks (ignored when --db-path is set)")
	dbPath := flag.String("db-path", envOr("DINGDONG_DB_PATH", ""), "SQLite database path; empty = in-memory backend")
	retentionRows := flag.Int("retention-rows", 100000, "max rows retained in SQLite before background trim (only with --db-path)")
	flag.Parse()

	token := os.Getenv("DINGDONG_TOKEN")
	if token == "" {
		log.Fatal("DINGDONG_TOKEN must be set")
	}

	store, backendKind, err := buildStore(*dbPath, *capacity, *retentionRows)
	if err != nil {
		log.Fatalf("init store: %v", err)
	}
	srv := server.New(server.Config{Token: token, Store: store})

	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           srv,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		log.Printf("dingdong listening on %s (backend=%s)", *addr, backendKind)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Println("shutting down")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
	if err := srv.Close(); err != nil {
		log.Printf("close store: %v", err)
	}
}

// buildStore picks the right backend based on whether --db-path was set.
// Returns the wrapped Store, a human-readable backend label for logging,
// and any open error.
func buildStore(dbPath string, capacity, retentionRows int) (*server.Store, string, error) {
	if dbPath == "" {
		return server.NewMemStore(capacity), fmt.Sprintf("mem cap=%d", capacity), nil
	}
	store, err := server.NewSQLiteStore(dbPath, retentionRows)
	if err != nil {
		return nil, "", err
	}
	return store, fmt.Sprintf("sqlite path=%s retention-rows=%d", dbPath, retentionRows), nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
