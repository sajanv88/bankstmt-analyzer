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
	Config config.Config
	Logger *slog.Logger
	DB     Pinger
}

func (d Deps) validate() error {
	if d.Logger == nil {
		return errors.New("http: Deps.Logger is required")
	}
	if d.DB == nil {
		return errors.New("http: Deps.DB is required")
	}
	return nil
}

// NewRouter builds the fully wired HTTP handler for the API.
func NewRouter(deps Deps) (http.Handler, error) {
	if err := deps.validate(); err != nil {
		return nil, err
	}

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(requestLogger(deps.Logger))
	r.Use(recoverer(deps.Logger))
	r.Use(cors.Handler(corsOptions(deps.Config)))

	health := newHealthHandler(deps.DB, deps.Logger)
	r.Get("/healthz", health.live)
	r.Get("/readyz", health.ready)

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
