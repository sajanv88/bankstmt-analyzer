// Command bankstmt-analyzer runs the bank statement analysis service: the
// HTTP API, the background worker, or both in one process.
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

	"golang.org/x/sync/errgroup"

	"github.com/sajanv88/bankstmt-analyzer/internal/config"
	"github.com/sajanv88/bankstmt-analyzer/internal/db"
	apihttp "github.com/sajanv88/bankstmt-analyzer/internal/http"
	"github.com/sajanv88/bankstmt-analyzer/internal/logging"
	"github.com/sajanv88/bankstmt-analyzer/internal/migrate"
	"github.com/sajanv88/bankstmt-analyzer/internal/storage"
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
	if opts.runWorker {
		return errors.New("worker mode requires the analysis pipeline, which is not part of this build")
	}

	blobs, err := storage.NewLocal(cfg.StorageDir)
	if err != nil {
		return err
	}
	logger.InfoContext(ctx, "blob storage ready", "root", blobs.Root())

	g, gctx := errgroup.WithContext(ctx)
	if opts.runAPI {
		if err := startAPI(gctx, g, cfg, logger, pool); err != nil {
			return err
		}
	}

	if err := g.Wait(); err != nil {
		return err
	}
	logger.InfoContext(context.WithoutCancel(ctx), "shutdown complete")
	return nil
}

// startAPI builds the HTTP server and registers its serve and shutdown
// goroutines with g.
func startAPI(ctx context.Context, g *errgroup.Group, cfg config.Config, logger *slog.Logger, pool apihttp.Pinger) error {
	handler, err := apihttp.NewRouter(apihttp.Deps{
		Config: cfg,
		Logger: logger,
		DB:     pool,
	})
	if err != nil {
		return err
	}

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
