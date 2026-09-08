package http

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"

	"github.com/sajanv88/bankstmt-analyzer/internal/config"
	"github.com/sajanv88/bankstmt-analyzer/internal/storage"
)

// Pinger is the slice of *pgxpool.Pool the readiness probe depends on.
// Depending on the method rather than the concrete pool keeps the router
// testable without a database.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Deps are the collaborators NewRouter needs. Everything is injected; the
// package holds no package-level state.
type Deps struct {
	Config   config.Config
	Logger   *slog.Logger
	DB       Pinger
	Store    UploadStore
	Blobs    storage.BlobStore
	Enqueuer Enqueuer
}

func (d Deps) validate() error {
	switch {
	case d.Logger == nil:
		return errors.New("http: Deps.Logger is required")
	case d.DB == nil:
		return errors.New("http: Deps.DB is required")
	case d.Store == nil:
		return errors.New("http: Deps.Store is required")
	case d.Blobs == nil:
		return errors.New("http: Deps.Blobs is required")
	case d.Enqueuer == nil:
		return errors.New("http: Deps.Enqueuer is required")
	default:
		return nil
	}
}

// NewRouter builds the fully wired HTTP handler for the API.
func NewRouter(deps Deps) (http.Handler, error) {
	if err := deps.validate(); err != nil {
		return nil, err
	}

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	// chi's RealIP is deliberately not used: it rewrites RemoteAddr from
	// client-supplied X-Forwarded-For / X-Real-IP headers whether or not a
	// trusted proxy set them, so any caller can forge the address that
	// ends up in the logs (GHSA-3fxj-6jh8-hvhx). The logged address is the
	// real peer instead. A deployment behind a trusted proxy that needs
	// the originating IP should resolve it against a configured list of
	// trusted hops rather than by trusting the header outright.
	r.Use(requestLogger(deps.Logger))
	r.Use(recoverer(deps.Logger))
	r.Use(cors.Handler(corsOptions(deps.Config)))

	health := newHealthHandler(deps.DB, deps.Logger)
	r.Get("/healthz", health.live)
	r.Get("/readyz", health.ready)

	uploads := newUploadHandler(deps.Store, deps.Blobs, deps.Enqueuer, deps.Config.Upload, deps.Logger)
	visualization := newVisualizationHandler(deps.Store, deps.Logger)

	r.Route("/api/v1", func(r chi.Router) {
		r.Route("/uploads", func(r chi.Router) {
			r.Post("/", uploads.create)
			r.Get("/{id}/status", uploads.status)
			r.Get("/{id}/visualization", visualization.get)
		})
	})

	mountSwagger(r, deps)

	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		WriteProblem(w, r, deps.Logger, NewProblem(http.StatusNotFound, "The requested resource does not exist."))
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		WriteProblem(w, r, deps.Logger, NewProblem(http.StatusMethodNotAllowed, "That method is not allowed on this resource."))
	})

	return r, nil
}

func corsOptions(cfg config.Config) cors.Options {
	return cors.Options{
		AllowedOrigins: cfg.HTTP.CORSAllowedOrigins,
		AllowedMethods: []string{http.MethodGet, http.MethodPost, http.MethodOptions},
		AllowedHeaders: []string{"Accept", "Authorization", "Content-Type", "X-Request-Id"},
		ExposedHeaders: []string{"X-Request-Id"},
		MaxAge:         300,
	}
}
