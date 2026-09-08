// Command bankstmt-analyzer runs the bank statement analysis service: the
// HTTP API, the background worker, or both in one process.
//
//	@title						bankstmt-analyzer API
//	@version					1.0
//	@description				Ingests bank statement PDFs, runs them through an Azure-hosted Mistral OCR endpoint and an Azure OpenAI deployment, and serves the resulting analysis and chart data.
//	@description
//	@description				Errors are returned as RFC 7807 problem documents with the media type application/problem+json.
//	@BasePath					/
//	@accept						json
//	@produce					json
//	@tag.name					uploads
//	@tag.description			Submitting statements and reading their analysis
//	@tag.name					health
//	@tag.description			Liveness and readiness probes
//
//	@securityDefinitions.apikey	ApiKeyAuth
//	@in							header
//	@name						api-key
//	@description				32 hexadecimal characters, as produced by `openssl rand -hex 16`. Required on every /api/v1 endpoint. The liveness and readiness probes do not take it.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	taskq "github.com/Nuvraxis/taskQ"
	"github.com/Nuvraxis/taskQ/pgbroker"
	"golang.org/x/sync/errgroup"

	"github.com/sajanv88/bankstmt-analyzer/internal/config"
	"github.com/sajanv88/bankstmt-analyzer/internal/db"
	apihttp "github.com/sajanv88/bankstmt-analyzer/internal/http"
	"github.com/sajanv88/bankstmt-analyzer/internal/llm"
	"github.com/sajanv88/bankstmt-analyzer/internal/logging"
	"github.com/sajanv88/bankstmt-analyzer/internal/migrate"
	"github.com/sajanv88/bankstmt-analyzer/internal/ocr"
	"github.com/sajanv88/bankstmt-analyzer/internal/pipeline"
	"github.com/sajanv88/bankstmt-analyzer/internal/storage"
	"github.com/sajanv88/bankstmt-analyzer/prompts"
)

// version is stamped at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		// The structured logger may not exist yet if configuration
		// failed, so the last-resort report goes straight to stderr.
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

// options is the parsed command line.
type options struct {
	runAPI      bool
	runWorker   bool
	migrate     bool
	showVersion bool
}

// parseFlags turns argv into options. Passing neither --api nor --worker
// selects both, which is what a single-container deployment wants; passing
// either one narrows the process to that role.
func parseFlags(args []string) (options, error) {
	fs := flag.NewFlagSet("bankstmt-analyzer", flag.ContinueOnError)
	api := fs.Bool("api", false, "run the HTTP API (default: run both API and worker)")
	worker := fs.Bool("worker", false, "run the background worker (default: run both API and worker)")
	doMigrate := fs.Bool("migrate", false, "apply database migrations during startup")
	showVersion := fs.Bool("version", false, "print the version and exit")

	if err := fs.Parse(args); err != nil {
		return options{}, fmt.Errorf("parse flags: %w", err)
	}

	explicit := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	if !explicit["api"] && !explicit["worker"] {
		*api, *worker = true, true
	}

	return options{
		runAPI:      *api,
		runWorker:   *worker,
		migrate:     *doMigrate,
		showVersion: *showVersion,
	}, nil
}

func run(args []string, stdout *os.File) error {
	opts, err := parseFlags(args)
	if err != nil {
		return err
	}
	if opts.showVersion {
		fmt.Fprintln(stdout, version)
		return nil
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logger := logging.New(stdout, cfg.LogLevel).With(
		"service", "bankstmt-analyzer",
		"version", version,
		"env", cfg.Env,
	)

	// SIGTERM is what Kubernetes sends before the grace period; SIGINT is
	// what Ctrl-C sends locally. Both cancel ctx and start the shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := db.NewPool(ctx, cfg.DatabaseURL, db.PoolConfig{})
	if err != nil {
		return err
	}
	defer pool.Close()
	logger.InfoContext(ctx, "connected to database")

	if opts.migrate || cfg.MigrateOnStart {
		if err := migrate.Up(ctx, pool, logger); err != nil {
			return err
		}
	}

	if !opts.runAPI && !opts.runWorker {
		// Selecting no role is how the Helm pre-upgrade Job asks for
		// migrations only: --migrate --api=false --worker=false.
		logger.InfoContext(ctx, "no run mode selected, exiting")
		return nil
	}

	blobs, err := newBlobStore(ctx, cfg, logger)
	if err != nil {
		return err
	}

	store := db.NewStore(pool)

	// The broker is built in both roles: the API needs it to enqueue and
	// the worker to consume. It borrows the same pool and never closes it.
	broker := pgbroker.New(pool,
		pgbroker.WithLeaseDuration(cfg.Queue.LeaseDuration),
		pgbroker.WithPollInterval(cfg.Queue.PollInterval),
	)

	analysis, err := newPipeline(cfg, logger, store, blobs, broker)
	if err != nil {
		return err
	}

	g, gctx := errgroup.WithContext(ctx)
	if opts.runAPI {
		if err := startAPI(gctx, g, apihttp.Deps{
			Config:   cfg,
			Logger:   logger,
			DB:       store,
			Store:    store,
			Blobs:    blobs,
			Enqueuer: analysis,
		}); err != nil {
			return err
		}
	}
	if opts.runWorker {
		g.Go(func() error { return analysis.Run(gctx) })
	}

	if err := g.Wait(); err != nil {
		return err
	}
	logger.InfoContext(context.WithoutCancel(ctx), "shutdown complete")
	return nil
}

// startAPI builds the HTTP server and registers its serve and shutdown
// goroutines with g.
func startAPI(ctx context.Context, g *errgroup.Group, deps apihttp.Deps) error {
	handler, err := apihttp.NewRouter(deps)
	if err != nil {
		return err
	}
	cfg, logger := deps.Config, deps.Logger

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           handler,
		ReadHeaderTimeout: cfg.HTTP.ReadHeaderTimeout,
		WriteTimeout:      cfg.HTTP.WriteTimeout,
		IdleTimeout:       cfg.HTTP.IdleTimeout,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}

	g.Go(func() error {
		logger.InfoContext(ctx, "http server listening", "addr", cfg.HTTPAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("http server: %w", err)
		}
		return nil
	})

	g.Go(func() error {
		<-ctx.Done()
		logger.Info("http server shutting down", "timeout", cfg.HTTP.ShutdownTimeout)
		// ctx is already cancelled and Shutdown needs a live deadline to
		// drain in-flight requests against. WithoutCancel keeps the
		// context's values while dropping only its cancellation.
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.HTTP.ShutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("http server shutdown: %w", err)
		}
		return nil
	})
	return nil
}

// newBlobStore builds the configured BlobStore.
//
// The object store is the right choice whenever the API and the worker run
// as separate processes: the API writes an upload's PDFs and the worker
// reads them, and with the local backend that only works if both see the
// same filesystem.
func newBlobStore(ctx context.Context, cfg config.Config, logger *slog.Logger) (storage.BlobStore, error) {
	switch cfg.Storage.Backend {
	case config.BackendS3:
		store, err := storage.NewS3(ctx, cfg.S3)
		if err != nil {
			return nil, err
		}
		logger.InfoContext(ctx, "blob storage ready",
			"backend", config.BackendS3,
			"bucket", store.Bucket(),
			"endpoint", cfg.S3.Endpoint,
		)
		return store, nil

	case config.BackendLocal:
		store, err := storage.NewLocal(cfg.StorageDir)
		if err != nil {
			return nil, err
		}
		logger.InfoContext(ctx, "blob storage ready",
			"backend", config.BackendLocal,
			"root", store.Root(),
		)
		return store, nil

	default:
		// config.Load rejects anything else, so reaching this means the
		// two have drifted apart.
		return nil, fmt.Errorf("unsupported storage backend %q", cfg.Storage.Backend)
	}
}

// newPipeline builds the analysis saga and the upstream clients it drives.
//
// It is constructed even when only the API is running: the API is the
// producer side of the same saga, and Enqueue is how an accepted upload
// reaches the queue.
func newPipeline(
	cfg config.Config,
	logger *slog.Logger,
	store *db.Store,
	blobs storage.BlobStore,
	broker taskq.Broker,
) (*pipeline.Pipeline, error) {
	ocrClient, err := ocr.NewClient(cfg.OCR)
	if err != nil {
		return nil, err
	}
	llmClient, err := llm.NewClient(cfg.OpenAI, prompts.AnalysisSystem)
	if err != nil {
		return nil, err
	}

	return pipeline.New(pipeline.Deps{
		Broker:  broker,
		Store:   store,
		Blobs:   blobs,
		OCR:     ocrClient,
		LLM:     llmClient,
		Logger:  logger,
		Queue:   cfg.Queue,
		Worker:  cfg.Worker,
		Secrets: cfg.Secrets(),
	})
}
