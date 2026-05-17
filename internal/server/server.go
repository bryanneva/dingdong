package server

import (
	"io/fs"
	"net/http"

	"github.com/bryanneva/dingdong/internal/ui"
)

type Config struct {
	Token string
	// Cap controls the in-memory ring-buffer fallback when Store is nil.
	// Ignored when Store is set. Defaults to 1000 if both are zero.
	Cap int
	// Store is the composite of a Backend + the live subscriber hub. When nil,
	// New constructs a memBackend-backed Store with capacity Cap — useful for
	// tests and local development. Production callers pass a durable Store.
	Store *Store
}

type Server struct {
	cfg   Config
	store *Store
	mux   *http.ServeMux
}

func New(cfg Config) *Server {
	if cfg.Store == nil {
		if cfg.Cap <= 0 {
			cfg.Cap = 1000
		}
		cfg.Store = NewMemStore(cfg.Cap)
	}
	s := &Server{
		cfg:   cfg,
		store: cfg.Store,
		mux:   http.NewServeMux(),
	}
	s.routes()
	return s
}

// Close releases the underlying store so the durable backend can flush and
// any live SSE subscribers unblock.
func (s *Server) Close() error {
	return s.store.Close()
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	s.mux.Handle("POST /v1/knocks", s.requireAuth(http.HandlerFunc(s.handlePostKnock)))
	s.mux.Handle("GET /v1/knocks", s.requireAuth(http.HandlerFunc(s.handleListKnocks)))
	s.mux.Handle("GET /v1/topics", s.requireAuth(http.HandlerFunc(s.handleListTopics)))
	s.mux.Handle("GET /v1/stream", s.requireAuth(http.HandlerFunc(s.handleStream)))

	// Static assets are served unauthenticated so the browser can load the HTML
	// + JS that prompts for a token. The bytes themselves contain no secrets;
	// the JS gates everything by attaching the token to /v1/* requests.
	static, err := fs.Sub(ui.Static, "static")
	if err != nil {
		panic(err)
	}
	s.mux.Handle("GET /", http.FileServer(http.FS(static)))
}
