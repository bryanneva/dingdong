package server

import (
	"io/fs"
	"net/http"
	"sync"

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
	cfg           Config
	store         *Store
	mux           *http.ServeMux
	dispatcher    *Dispatcher
	deliveryWG    sync.WaitGroup
	deliveryQueue *DeliveryQueue // non-nil when using a WebhookBackend (SQLite mode)
}

func New(cfg Config) *Server {
	if cfg.Store == nil {
		if cfg.Cap <= 0 {
			cfg.Cap = 1000
		}
		cfg.Store = NewMemStore(cfg.Cap)
	}
	s := &Server{
		cfg:        cfg,
		store:      cfg.Store,
		mux:        http.NewServeMux(),
		dispatcher: NewDispatcher(),
	}
	if s.store.webhookDB != nil {
		s.deliveryQueue = NewDeliveryQueue(s.store.webhookDB, s.dispatcher)
		s.deliveryQueue.Start()
	}
	s.routes()
	return s
}

// Close stops the delivery queue (waits for in-flight dispatches), then
// releases the underlying store so the durable backend can flush and any
// live SSE subscribers unblock.
func (s *Server) Close() error {
	if s.deliveryQueue != nil {
		s.deliveryQueue.Stop()
	}
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

	s.mux.Handle("POST /v1/webhooks", s.requireAuth(http.HandlerFunc(s.handleCreateWebhook)))
	s.mux.Handle("GET /v1/webhooks", s.requireAuth(http.HandlerFunc(s.handleListWebhooks)))
	s.mux.Handle("DELETE /v1/webhooks/{id}", s.requireAuth(http.HandlerFunc(s.handleDeleteWebhook)))

	// Static assets are served unauthenticated so the browser can load the HTML
	// + JS that prompts for a token. The bytes themselves contain no secrets;
	// the JS gates everything by attaching the token to /v1/* requests.
	static, err := fs.Sub(ui.Static, "static")
	if err != nil {
		panic(err)
	}
	s.mux.Handle("GET /", http.FileServer(http.FS(static)))
}
