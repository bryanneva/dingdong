package main

import (
	"context"
	"flag"
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
	capacity := flag.Int("capacity", 1000, "max in-memory knocks (ring buffer size)")
	flag.Parse()

	token := os.Getenv("DINGDONG_TOKEN")
	if token == "" {
		log.Fatal("DINGDONG_TOKEN must be set")
	}

	srv := server.New(server.Config{Token: token, Cap: *capacity})

	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           srv,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		log.Printf("dingdong listening on %s (capacity=%d)", *addr, *capacity)
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
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
