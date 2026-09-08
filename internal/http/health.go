package http

import (
	"context"
	"log/slog"
	"net/http"
	"time"
)

// readinessTimeout bounds the database ping behind /readyz so a wedged
// database turns the probe red instead of hanging it.
const readinessTimeout = 2 * time.Second

// HealthResponse is the body of a successful health probe.
type HealthResponse struct {
	Status string `json:"status" example:"ok"`
}

type healthHandler struct {
	db     Pinger
	logger *slog.Logger
}

func newHealthHandler(db Pinger, logger *slog.Logger) *healthHandler {
	return &healthHandler{db: db, logger: logger}
}

// live reports process liveness.
//
//	@Summary		Liveness probe
//	@Description	Reports that the process is running. It performs no dependency checks, so it never fails because of a database outage.
//	@Tags			health
//	@Produce		json
//	@Success		200	{object}	HealthResponse
//	@Router			/healthz [get]
func (h *healthHandler) live(w http.ResponseWriter, r *http.Request) {
	WriteJSON(w, r, h.logger, http.StatusOK, HealthResponse{Status: "ok"})
}

// ready reports readiness to serve traffic.
//
//	@Summary		Readiness probe
//	@Description	Reports whether the service can serve traffic, verified by pinging the database.
//	@Tags			health
//	@Produce		json
//	@Success		200	{object}	HealthResponse
//	@Failure		503	{object}	Problem
//	@Router			/readyz [get]
func (h *healthHandler) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), readinessTimeout)
	defer cancel()

	if err := h.db.Ping(ctx); err != nil {
		h.logger.WarnContext(ctx, "readiness probe failed", "error", err)
		WriteProblem(w, r, h.logger, NewProblem(http.StatusServiceUnavailable, "The database is not reachable."))
		return
	}
	WriteJSON(w, r, h.logger, http.StatusOK, HealthResponse{Status: "ok"})
}
