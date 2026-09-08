package config_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sajanv88/bankstmt-analyzer/internal/config"
)

// requiredEnv is the minimum set of variables the service refuses to start
// without.
func requiredEnv() map[string]string {
	return map[string]string{
		"DATABASE_URL":            "postgres://user:pass@localhost:5432/db",
		"API_KEY":                 "0123456789abcdef0123456789abcdef",
		"STORAGE_DIR":             "/var/lib/bankstmt",
		"AZURE_OCR_ENDPOINT":      "https://ocr.example.com",
		"AZURE_OCR_API_KEY":       "ocr-key",
		"AZURE_OCR_MODEL":         "mistral-ocr-2503",
		"AZURE_OPENAI_ENDPOINT":   "https://openai.example.com",
		"AZURE_OPENAI_API_KEY":    "openai-key",
		"AZURE_OPENAI_DEPLOYMENT": "gpt-4o",
	}
}

// managedEnv is every variable config.Load reads. Each test blanks all of
// them before setting what it needs, so a result never depends on what
// happened to be exported in the surrounding shell. That is not
// hypothetical: CI sets S3_ENDPOINT and the S3 credentials for the storage
// conformance suite, and without this the backend validation tests would
// find credentials they were asserting were absent.
var managedEnv = []string{
	"ENV", "HTTP_ADDR", "LOG_LEVEL", "DATABASE_URL", "MIGRATE_ON_START", "API_KEY",
	"STORAGE_BACKEND", "STORAGE_DIR",
	"S3_ENDPOINT", "S3_BUCKET", "S3_REGION", "S3_ACCESS_KEY_ID",
	"S3_SECRET_ACCESS_KEY", "S3_USE_PATH_STYLE", "S3_PREFIX", "S3_TIMEOUT",
	"AZURE_OCR_ENDPOINT", "AZURE_OCR_API_KEY", "AZURE_OCR_MODEL",
	"AZURE_OCR_PATH", "AZURE_OCR_TIMEOUT",
	"AZURE_OPENAI_ENDPOINT", "AZURE_OPENAI_API_KEY", "AZURE_OPENAI_DEPLOYMENT",
	"AZURE_OPENAI_API_VERSION", "AZURE_OPENAI_TIMEOUT",
	"HTTP_CORS_ALLOWED_ORIGINS",
	"WORKER_CONCURRENCY", "WORKER_MAX_RETRY",
	"UPLOAD_MAX_FILES", "UPLOAD_MAX_FILE_BYTES",
	"TASKQ_QUEUE", "TASKQ_LEASE_DURATION",
}

// setEnv blanks every managed variable, then applies env. t.Setenv
// restores the previous values when the test ends.
func setEnv(t *testing.T, env map[string]string) {
	t.Helper()
	for _, name := range managedEnv {
		t.Setenv(name, "")
	}
	for k, v := range env {
		t.Setenv(k, v)
	}
}

// addEnv applies more variables without blanking what is already set.
func addEnv(t *testing.T, env map[string]string) {
	t.Helper()
	for k, v := range env {
		t.Setenv(k, v)
	}
}

func TestLoadAppliesDefaults(t *testing.T) {
	setEnv(t, requiredEnv())

	cfg, err := config.Load()
	require.NoError(t, err)

	assert.Equal(t, ":8080", cfg.HTTPAddr)
	assert.Equal(t, config.EnvDevelopment, cfg.Env)
	assert.False(t, cfg.IsProduction())
	assert.False(t, cfg.MigrateOnStart)
	assert.Equal(t, 12, cfg.Upload.MaxFiles)
	assert.Equal(t, int64(20*1024*1024), cfg.Upload.MaxFileBytes)
	assert.Equal(t, "bankstmt-analysis", cfg.Queue.Name)
	assert.Equal(t, 15*time.Minute, cfg.Queue.LeaseDuration)
	assert.Equal(t, 4, cfg.Worker.Concurrency)
	assert.Equal(t, []string{"*"}, cfg.HTTP.CORSAllowedOrigins)
}

func TestLoadReadsPrefixedAzureSettings(t *testing.T) {
	setEnv(t, requiredEnv())
	t.Setenv("ENV", config.EnvProduction)
	t.Setenv("MIGRATE_ON_START", "true")
	t.Setenv("HTTP_CORS_ALLOWED_ORIGINS", "https://a.example.com,https://b.example.com")

	cfg, err := config.Load()
	require.NoError(t, err)

	assert.True(t, cfg.IsProduction())
	assert.True(t, cfg.MigrateOnStart)
	assert.Equal(t, "https://ocr.example.com", cfg.OCR.Endpoint)
	assert.Equal(t, "mistral-ocr-2503", cfg.OCR.Model)
	assert.Equal(t, "gpt-4o", cfg.OpenAI.Deployment)
	assert.Equal(t, []string{"https://a.example.com", "https://b.example.com"}, cfg.HTTP.CORSAllowedOrigins)
}

func TestLoadRequiresEachRequiredVariable(t *testing.T) {
	for missing := range requiredEnv() {
		t.Run("missing "+missing, func(t *testing.T) {
			env := requiredEnv()
			delete(env, missing)
			setEnv(t, env)
			// t.Setenv cannot unset, so blank the omitted one explicitly;
			// every required var is also marked notEmpty.
			t.Setenv(missing, "")

			_, err := config.Load()
			require.Error(t, err)
		})
	}
}

func TestSecretsListsCredentialsToScrub(t *testing.T) {
	setEnv(t, requiredEnv())

	cfg, err := config.Load()
	require.NoError(t, err)

	assert.ElementsMatch(t,
		[]string{
			"ocr-key", "openai-key", "postgres://user:pass@localhost:5432/db",
			// The API key is scrubbed too: an upstream error that echoed a
			// rejected header would otherwise store it in failure_reason.
			"0123456789abcdef0123456789abcdef",
		},
		cfg.Secrets(),
	)
}

func TestLoadRejectsOutOfRangeTunables(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
	}{
		{name: "zero max files", env: map[string]string{"UPLOAD_MAX_FILES": "0"}},
		{name: "zero max bytes", env: map[string]string{"UPLOAD_MAX_FILE_BYTES": "0"}},
		{name: "zero concurrency", env: map[string]string{"WORKER_CONCURRENCY": "0"}},
		{name: "negative retries", env: map[string]string{"WORKER_MAX_RETRY": "-1"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setEnv(t, requiredEnv())
			addEnv(t, tc.env)

			_, err := config.Load()
			require.Error(t, err)
		})
	}
}

func TestStorageBackendValidation(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantErr string
	}{
		{
			name: "local is the default and needs a directory",
			env:  map[string]string{"STORAGE_DIR": ""},
			// STORAGE_DIR cannot be marked required in the struct tags
			// because it only matters for one backend.
			wantErr: "STORAGE_DIR is required",
		},
		{
			name: "s3 needs a bucket and credentials",
			env: map[string]string{
				"STORAGE_BACKEND": config.BackendS3,
				"STORAGE_DIR":     "",
			},
			wantErr: "S3_BUCKET, S3_ACCESS_KEY_ID, S3_SECRET_ACCESS_KEY required",
		},
		{
			name: "s3 needs the credentials even with a bucket",
			env: map[string]string{
				"STORAGE_BACKEND": config.BackendS3,
				"STORAGE_DIR":     "",
				"S3_BUCKET":       "uploads",
			},
			wantErr: "S3_ACCESS_KEY_ID, S3_SECRET_ACCESS_KEY required",
		},
		{
			name:    "an unknown backend is rejected",
			env:     map[string]string{"STORAGE_BACKEND": "azure-blob"},
			wantErr: `must be "local" or "s3"`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setEnv(t, requiredEnv())
			addEnv(t, tc.env)

			_, err := config.Load()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// TestStorageBackendS3NeedsNoDirectory proves the two backends require
// disjoint settings: choosing s3 must not also demand STORAGE_DIR.
func TestStorageBackendS3NeedsNoDirectory(t *testing.T) {
	setEnv(t, requiredEnv())
	addEnv(t, map[string]string{
		"STORAGE_BACKEND":      config.BackendS3,
		"STORAGE_DIR":          "",
		"S3_ENDPOINT":          "http://minio:9000",
		"S3_BUCKET":            "uploads",
		"S3_ACCESS_KEY_ID":     "access",
		"S3_SECRET_ACCESS_KEY": "secret-access-key",
	})

	cfg, err := config.Load()
	require.NoError(t, err)

	assert.Equal(t, config.BackendS3, cfg.Storage.Backend)
	assert.Equal(t, "uploads", cfg.S3.Bucket)
	assert.True(t, cfg.S3.UsePathStyle, "MinIO needs path-style addressing")
	assert.Equal(t, "us-east-1", cfg.S3.Region)
	assert.Contains(t, cfg.Secrets(), "secret-access-key",
		"the S3 secret must be scrubbed from errors like the other credentials")
}

func TestLoadRejectsAnAPIKeyThatIsNot32HexCharacters(t *testing.T) {
	tests := []struct {
		name string
		key  string
	}{
		{"too short", "0123456789abcdef"},
		{"too long", "0123456789abcdef0123456789abcdef00"},
		{"not hexadecimal", "0123456789abcdef0123456789abcdeg"},
		{"32 characters of the wrong alphabet", "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"},
		{"empty", ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setEnv(t, requiredEnv())
			addEnv(t, map[string]string{"API_KEY": tc.key})

			_, err := config.Load()

			require.Error(t, err)
			assert.Contains(t, err.Error(), "API_KEY")
			// The rejected value must not be quoted back into the error:
			// it is a credential, and configuration errors get logged.
			if tc.key != "" {
				assert.NotContains(t, err.Error(), tc.key)
			}
		})
	}
}

func TestLoadAcceptsUppercaseHexAPIKey(t *testing.T) {
	setEnv(t, requiredEnv())
	addEnv(t, map[string]string{"API_KEY": "0123456789ABCDEF0123456789ABCDEF"})

	cfg, err := config.Load()

	require.NoError(t, err)
	assert.Equal(t, "0123456789ABCDEF0123456789ABCDEF", cfg.APIKey)
}
