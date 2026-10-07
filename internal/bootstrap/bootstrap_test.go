package bootstrap

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"go.uber.org/fx"
	"go.uber.org/fx/fxtest"

	"github.com/gabrielmilano/wager-wallet-service/internal/adapter/httpapi"
	"github.com/gabrielmilano/wager-wallet-service/internal/platform/config"
)

func testConfig() config.Config {
	return config.Config{
		HTTPAddr:        "127.0.0.1:0",
		InstanceID:      "test-1",
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

// quiet troca o logger por um que descarta tudo, para não poluir a saída
// dos testes.
var quiet = fx.Decorate(func() *slog.Logger { return slog.New(slog.DiscardHandler) })

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

// TestAppStartsAndStops sobe a aplicação de verdade, confere que o HTTP
// responde e que, depois do Stop, a porta foi liberada.
func TestAppStartsAndStops(t *testing.T) {
	var srv *httpapi.Server
	app := fxtest.New(t, Options(testConfig()), quiet, fx.Populate(&srv))

	app.RequireStart()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	addr := srv.Addr().String()
	if err := httpapi.Probe(ctx, addr); err != nil {
		t.Errorf("Probe com a aplicação no ar: %v", err)
	}

	app.RequireStop()

	if err := httpapi.Probe(ctx, addr); err == nil {
		t.Error("Probe depois do Stop: esperado erro, veio nil")
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
