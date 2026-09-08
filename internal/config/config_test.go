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
		"STORAGE_DIR":             "/var/lib/bankstmt",
		"AZURE_OCR_ENDPOINT":      "https://ocr.example.com",
		"AZURE_OCR_API_KEY":       "ocr-key",
		"AZURE_OCR_MODEL":         "mistral-ocr-2503",
		"AZURE_OPENAI_ENDPOINT":   "https://openai.example.com",
		"AZURE_OPENAI_API_KEY":    "openai-key",
		"AZURE_OPENAI_DEPLOYMENT": "gpt-4o",
	}
}

func setEnv(t *testing.T, env map[string]string) {
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
		[]string{"ocr-key", "openai-key", "postgres://user:pass@localhost:5432/db"},
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
			setEnv(t, tc.env)

			_, err := config.Load()
			require.Error(t, err)
		})
	}
}
