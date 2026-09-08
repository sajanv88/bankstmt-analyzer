package http

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	httpSwagger "github.com/swaggo/http-swagger/v2"

	// Registers the generated OpenAPI document with the swag registry that
	// httpSwagger reads from. Regenerate it with `make swagger` after
	// changing any handler annotation or DTO.
	_ "github.com/sajanv88/bankstmt-analyzer/docs"
)

// swaggerPrefix is where the UI and the generated document are served.
const swaggerPrefix = "/swagger"

// mountSwagger exposes the API documentation, but never in production: the
// document enumerates every endpoint and shape the service accepts, which
// is a development aid rather than something a public deployment needs to
// publish.
func mountSwagger(r chi.Router, deps Deps) {
	if deps.Config.IsProduction() {
		return
	}

	r.Get(swaggerPrefix+"/*", httpSwagger.Handler(
		httpSwagger.URL(swaggerPrefix+"/doc.json"),
	))
	// Bare /swagger is what people type; without this it would fall
	// through to the 404 handler.
	r.Get(swaggerPrefix, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, swaggerPrefix+"/index.html", http.StatusMovedPermanently)
	})

	deps.Logger.Info("swagger UI enabled", "path", swaggerPrefix+"/index.html")
}
