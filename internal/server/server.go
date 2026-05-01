package server

import (
	"io/fs"
	"net/http"

	"github.com/bryanneva/dingdong/internal/ui"
)

type Config struct {
	Token string
	Cap   int
}

type Server struct {
	cfg   Config
	store *Store
	mux   *http.ServeMux
}

func New(cfg Config) *Server {
	if cfg.Cap <= 0 {
		cfg.Cap = 1000
	}
	s := &Server{
		cfg:   cfg,
		store: NewStore(cfg.Cap),
		mux:   http.NewServeMux(),
	}
	s.routes()
	return s
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
	s.mux.Handle("GET /v1/stream", s.requireAuth(http.HandlerFunc(s.handleStream)))

	static, err := fs.Sub(ui.Static, "static")
	if err != nil {
		panic(err)
	}
	s.mux.Handle("GET /", s.requireAuth(http.FileServer(http.FS(static))))
}
