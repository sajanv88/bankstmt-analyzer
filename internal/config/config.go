// Package config loads all runtime configuration from the environment.
// Nothing here reads flags or files: the process is configured entirely by
// env vars so the same binary behaves identically in docker-compose and in
// Kubernetes, where values arrive from a Secret or ConfigMap.
package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"
)

// Environment names that get special treatment elsewhere in the service.
const (
	EnvProduction  = "production"
	EnvDevelopment = "development"
)

// Config is the fully resolved configuration for one process, whether it
// runs the API, the worker, or both.
type Config struct {
	// Env names the deployment environment. Swagger is only served when
	// this is not "production".
	Env string `env:"ENV" envDefault:"development"`

	// HTTPAddr is the listen address for the API server.
	HTTPAddr string `env:"HTTP_ADDR" envDefault:":8080"`

	// LogLevel is one of debug, info, warn, error.
	LogLevel string `env:"LOG_LEVEL" envDefault:"info"`

	// DatabaseURL is the pgx connection string. It backs both the
	// application tables and the taskQ Postgres broker.
	DatabaseURL string `env:"DATABASE_URL,required,notEmpty"`

	// MigrateOnStart runs goose migrations during startup. The --migrate
	// flag sets the same behaviour; either one is enough.
	MigrateOnStart bool `env:"MIGRATE_ON_START" envDefault:"false"`

	// StorageDir is the directory backing the local BlobStore. Required
	// when Storage.Backend is "local", ignored otherwise.
	StorageDir string `env:"STORAGE_DIR"`

	Storage StorageConfig `envPrefix:"STORAGE_"`
	S3      S3Config      `envPrefix:"S3_"`

	OCR    AzureOCR    `envPrefix:"AZURE_OCR_"`
	OpenAI AzureOpenAI `envPrefix:"AZURE_OPENAI_"`

	HTTP   HTTPConfig   `envPrefix:"HTTP_"`
	Worker WorkerConfig `envPrefix:"WORKER_"`
	Queue  QueueConfig  `envPrefix:"TASKQ_"`
	Upload UploadConfig `envPrefix:"UPLOAD_"`
}

// Storage backend names.
const (
	// BackendLocal writes uploaded PDFs to a directory on disk. The API
	// and the worker must share that directory, so it only suits a
	// single-process deployment or a shared volume.
	BackendLocal = "local"
	// BackendS3 writes them to an S3-compatible object store: MinIO, AWS
	// S3, or anything else speaking the same API. Both roles reach the
	// same bucket over the network, so no shared filesystem is involved.
	BackendS3 = "s3"
)

// StorageConfig selects which BlobStore implementation to use.
type StorageConfig struct {
	Backend string `env:"BACKEND" envDefault:"local"`
}

// S3Config addresses an S3-compatible object store.
type S3Config struct {
	// Endpoint is the service URL, for example http://minio:9000. Leave
	// it empty to use AWS S3 proper, where the SDK resolves the endpoint
	// from the region.
	Endpoint string `env:"ENDPOINT"`
	Bucket   string `env:"BUCKET"`
	// Region is required by the signing algorithm even where the store
	// ignores it, which MinIO does.
	Region          string `env:"REGION" envDefault:"us-east-1"`
	AccessKeyID     string `env:"ACCESS_KEY_ID"`
	SecretAccessKey string `env:"SECRET_ACCESS_KEY"`
	// UsePathStyle addresses buckets as endpoint/bucket/key rather than
	// bucket.endpoint/key. MinIO and most other S3-compatible stores need
	// this; AWS S3 prefers it off.
	UsePathStyle bool `env:"USE_PATH_STYLE" envDefault:"true"`
	// Prefix namespaces every key, so one bucket can host several
	// environments.
	Prefix string `env:"PREFIX"`
	// Timeout bounds a single object operation.
	Timeout time.Duration `env:"TIMEOUT" envDefault:"2m"`
}

// AzureOCR addresses the Azure-hosted Mistral OCR deployment.
type AzureOCR struct {
	Endpoint string `env:"ENDPOINT,required,notEmpty"`
	APIKey   string `env:"API_KEY,required,notEmpty"`
	Model    string `env:"MODEL,required,notEmpty"`
	// Path is appended to Endpoint to form the OCR URL. It is
	// configurable because Azure has moved model-serving routes before,
	// and a changed route should not need a new build.
	Path string `env:"PATH" envDefault:"/providers/mistral/azure/ocr"`
	// Timeout bounds a single OCR call for one PDF.
	Timeout time.Duration `env:"TIMEOUT" envDefault:"5m"`
}

// AzureOpenAI addresses the Azure OpenAI chat completions deployment used
// for the statement analysis.
type AzureOpenAI struct {
	Endpoint   string `env:"ENDPOINT,required,notEmpty"`
	APIKey     string `env:"API_KEY,required,notEmpty"`
	Deployment string `env:"DEPLOYMENT,required,notEmpty"`
	APIVersion string `env:"API_VERSION" envDefault:"2024-10-21"`
	// Timeout bounds a single analysis call across all statements.
	Timeout time.Duration `env:"TIMEOUT" envDefault:"10m"`
}

// HTTPConfig holds server-side timeouts and CORS policy. The listen
// address lives on Config as HTTP_ADDR, which is the name the deployment
// contract specifies.
type HTTPConfig struct {
	ReadHeaderTimeout time.Duration `env:"READ_HEADER_TIMEOUT" envDefault:"10s"`
	// WriteTimeout is generous because uploads carry up to 12 PDFs.
	WriteTimeout    time.Duration `env:"WRITE_TIMEOUT" envDefault:"2m"`
	IdleTimeout     time.Duration `env:"IDLE_TIMEOUT" envDefault:"60s"`
	ShutdownTimeout time.Duration `env:"SHUTDOWN_TIMEOUT" envDefault:"20s"`
	// CORSAllowedOrigins defaults to "*" because the service exposes no
	// cookie-based session for a browser to leak.
	CORSAllowedOrigins []string `env:"CORS_ALLOWED_ORIGINS" envSeparator:"," envDefault:"*"`
}

// WorkerConfig tunes the taskQ worker pool.
type WorkerConfig struct {
	Concurrency int `env:"CONCURRENCY" envDefault:"4"`
	// MaxRetry is taskQ's per-hop retry budget: a saga step is attempted
	// MaxRetry+1 times before the run pivots to compensation.
	MaxRetry    int           `env:"MAX_RETRY" envDefault:"3"`
	BackoffBase time.Duration `env:"BACKOFF_BASE" envDefault:"2s"`
	BackoffMax  time.Duration `env:"BACKOFF_MAX" envDefault:"2m"`
}

// QueueConfig tunes the taskQ Postgres broker.
type QueueConfig struct {
	Name string `env:"QUEUE" envDefault:"bankstmt-analysis"`
	// LeaseDuration must comfortably exceed the slowest single saga hop
	// (an OCR or LLM round trip) or an in-flight message is redelivered
	// to a second worker while the first is still working on it.
	LeaseDuration time.Duration `env:"LEASE_DURATION" envDefault:"15m"`
	PollInterval  time.Duration `env:"POLL_INTERVAL" envDefault:"1s"`
}

// UploadConfig bounds what POST /api/v1/uploads accepts.
type UploadConfig struct {
	MaxFiles     int   `env:"MAX_FILES" envDefault:"12"`
	MaxFileBytes int64 `env:"MAX_FILE_BYTES" envDefault:"20971520"`
}

// Load reads and validates the configuration from the process environment.
func Load() (Config, error) {
	var cfg Config
	if err := env.Parse(&cfg); err != nil {
		return Config{}, fmt.Errorf("config: parse environment: %w", err)
	}
	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// IsProduction reports whether Swagger and other non-production surfaces
// should stay switched off.
func (c Config) IsProduction() bool { return c.Env == EnvProduction }

// Secrets returns the values that must never appear in a stored failure
// reason or a log line. The pipeline uses it to scrub upstream errors.
func (c Config) Secrets() []string {
	out := make([]string, 0, 4)
	for _, s := range []string{c.OCR.APIKey, c.OpenAI.APIKey, c.DatabaseURL, c.S3.SecretAccessKey} {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func (c Config) validate() error {
	if err := c.validateStorage(); err != nil {
		return err
	}
	if c.Upload.MaxFiles < 1 {
		return fmt.Errorf("config: UPLOAD_MAX_FILES must be >= 1, got %d", c.Upload.MaxFiles)
	}
	if c.Upload.MaxFileBytes < 1 {
		return fmt.Errorf("config: UPLOAD_MAX_FILE_BYTES must be >= 1, got %d", c.Upload.MaxFileBytes)
	}
	if c.Worker.Concurrency < 1 {
		return fmt.Errorf("config: WORKER_CONCURRENCY must be >= 1, got %d", c.Worker.Concurrency)
	}
	if c.Worker.MaxRetry < 0 {
		return fmt.Errorf("config: WORKER_MAX_RETRY must be >= 0, got %d", c.Worker.MaxRetry)
	}
	return nil
}

// validateStorage checks the settings the selected backend actually needs.
// They cannot be marked required in the struct tags because which ones
// matter depends on STORAGE_BACKEND.
func (c Config) validateStorage() error {
	switch c.Storage.Backend {
	case BackendLocal:
		if strings.TrimSpace(c.StorageDir) == "" {
			return fmt.Errorf("config: STORAGE_DIR is required when STORAGE_BACKEND is %q", BackendLocal)
		}
		return nil
	case BackendS3:
		var missing []string
		if strings.TrimSpace(c.S3.Bucket) == "" {
			missing = append(missing, "S3_BUCKET")
		}
		if strings.TrimSpace(c.S3.AccessKeyID) == "" {
			missing = append(missing, "S3_ACCESS_KEY_ID")
		}
		if strings.TrimSpace(c.S3.SecretAccessKey) == "" {
			missing = append(missing, "S3_SECRET_ACCESS_KEY")
		}
		if len(missing) > 0 {
			return fmt.Errorf("config: %s required when STORAGE_BACKEND is %q",
				strings.Join(missing, ", "), BackendS3)
		}
		return nil
	default:
		return fmt.Errorf("config: STORAGE_BACKEND must be %q or %q, got %q",
			BackendLocal, BackendS3, c.Storage.Backend)
	}
}
