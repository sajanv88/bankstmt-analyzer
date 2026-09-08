package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"

	"github.com/sajanv88/bankstmt-analyzer/internal/config"
)

func TestParseFlags(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		args       []string
		wantAPI    bool
		wantWorker bool
		wantMigrat bool
		wantErr    bool
	}{
		{
			name:       "no flags runs both roles",
			args:       nil,
			wantAPI:    true,
			wantWorker: true,
		},
		{
			name:       "--api narrows to the API",
			args:       []string{"--api"},
			wantAPI:    true,
			wantWorker: false,
		},
		{
			name:       "--worker narrows to the worker",
			args:       []string{"--worker"},
			wantAPI:    false,
			wantWorker: true,
		},
		{
			name:       "both flags run both roles",
			args:       []string{"--api", "--worker"},
			wantAPI:    true,
			wantWorker: true,
		},
		{
			name:       "--migrate alone still runs both roles",
			args:       []string{"--migrate"},
			wantAPI:    true,
			wantWorker: true,
			wantMigrat: true,
		},
		{
			name:       "explicitly disabling both roles is the migrate-only mode",
			args:       []string{"--migrate", "--api=false", "--worker=false"},
			wantAPI:    false,
			wantWorker: false,
			wantMigrat: true,
		},
		{
			name:    "an unknown flag is an error",
			args:    []string{"--nope"},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			opts, err := parseFlags(tc.args)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantAPI, opts.runAPI, "runAPI")
			assert.Equal(t, tc.wantWorker, opts.runWorker, "runWorker")
			assert.Equal(t, tc.wantMigrat, opts.migrate, "migrate")
		})
	}
}

// TestStartAPIShutsDownOnContextCancel exercises the graceful shutdown path
// directly rather than through a signal: Windows cannot deliver a catchable
// SIGTERM to a native process, so a signal-based test would prove nothing
// here even though the deployed target is Linux. Cancelling the context is
// exactly what signal.NotifyContext does once the signal arrives.
func TestStartAPIShutsDownOnContextCancel(t *testing.T) {
	addr := freeAddr(t)

	cfg := config.Config{
		Env:      config.EnvDevelopment,
		HTTPAddr: addr,
		HTTP: config.HTTPConfig{
			ReadHeaderTimeout:  time.Second,
			WriteTimeout:       5 * time.Second,
			IdleTimeout:        5 * time.Second,
			ShutdownTimeout:    5 * time.Second,
			CORSAllowedOrigins: []string{"*"},
		},
	}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))

	ctx, cancel := context.WithCancel(t.Context())
	g, gctx := errgroup.WithContext(ctx)
	require.NoError(t, startAPI(gctx, g, cfg, logger, okPinger{}))

	requireServing(t, "http://"+addr+"/healthz")

	cancel()

	done := make(chan error, 1)
	go func() { done <- g.Wait() }()

	select {
	case err := <-done:
		require.NoError(t, err, "a clean shutdown should not surface an error")
	case <-time.After(15 * time.Second):
		t.Fatal("the server did not shut down after its context was cancelled")
	}

	// The listener must actually be released, not merely stop being polled.
	dialCtx, dialCancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer dialCancel()
	var dialer net.Dialer
	_, err := dialer.DialContext(dialCtx, "tcp", addr)
	assert.Error(t, err, "the listener should be closed after shutdown")
}

type okPinger struct{}

func (okPinger) Ping(context.Context) error { return nil }

// freeAddr reserves an ephemeral port, then releases it so the server under
// test can bind it.
func freeAddr(t *testing.T) string {
	t.Helper()
	var lc net.ListenConfig
	l, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := l.Addr().String()
	require.NoError(t, l.Close())
	return addr
}

func requireServing(t *testing.T, url string) {
	t.Helper()
	var lastErr error
	for range 50 {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
		require.NoError(t, err)

		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			require.Equal(t, http.StatusOK, resp.StatusCode)
			return
		}
		lastErr = err
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("server never became reachable at %s: %v", url, lastErr)
}
