package bootstrap

import (
	"log/slog"
	"testing"
	"time"

	"go.uber.org/fx"

	"github.com/gabrielmilano/wager-wallet-service/internal/adapter/httpapi"
	"github.com/gabrielmilano/wager-wallet-service/internal/platform/config"
)

func testConfig() config.Config {
	return config.Config{
		HTTPAddr:        "127.0.0.1:0",
		InstanceID:      "test-1",
		DatabaseURL:     "postgres://app_runtime:x@localhost:5432/wager_wallet",
		DBMaxConns:      2,
		DBLockTimeout:   time.Second,
		ShutdownTimeout: 5 * time.Second,
		LogLevel:        slog.LevelInfo,
		Components: config.Components{
			HTTP:            true,
			SQSConsumer:     true,
			OutboxPublisher: true,
			PendingWorker:   true,
		},
	}
}

func TestOptionsValidate(t *testing.T) {
	tests := []struct {
		name       string
		components config.Components
	}{
		{"todos ligados", config.Components{HTTP: true, SQSConsumer: true, OutboxPublisher: true, PendingWorker: true}},
		{"sem HTTP", config.Components{SQSConsumer: true}},
		{"só HTTP", config.Components{HTTP: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.Components = tt.components
			if err := fx.ValidateApp(Options(cfg)); err != nil {
				t.Errorf("ValidateApp: %v", err)
			}
		})
	}
}

func TestAppWithoutHTTPHasNoServer(t *testing.T) {
	cfg := testConfig()
	cfg.Components.HTTP = false

	var srv *httpapi.Server
	err := fx.ValidateApp(Options(cfg), fx.Populate(&srv))
	if err == nil {
		t.Error("esperado erro: *httpapi.Server não deveria estar no grafo com HTTP desligado")
	}
}
