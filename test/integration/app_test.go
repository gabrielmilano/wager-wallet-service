//go:build integration

package integration

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"
	"go.uber.org/fx/fxtest"

	"github.com/gabrielmilano/wager-wallet-service/internal/adapter/httpapi"
	"github.com/gabrielmilano/wager-wallet-service/internal/bootstrap"
	"github.com/gabrielmilano/wager-wallet-service/internal/platform/config"
)

// appConfig carrega a configuração real da aplicação a partir do ambiente,
// com os padrões locais do .env.example e a porta HTTP efêmera.
func appConfig(t *testing.T, overrides map[string]string) config.Config {
	t.Helper()
	// O SDK da AWS lê as credenciais do ambiente (cadeia padrão); no Compose
	// elas vêm do docker-compose.yml.
	for k, def := range map[string]string{"AWS_ACCESS_KEY_ID": "test", "AWS_SECRET_ACCESS_KEY": "test"} {
		if env(k, "") == "" {
			t.Setenv(k, def)
		}
	}
	defaults := map[string]string{
		"APP_HTTP_ADDR":         "127.0.0.1:0",
		"APP_INSTANCE_ID":       "integration",
		"DATABASE_URL":          "postgres://app_runtime:app_runtime@localhost:5432/wager_wallet?sslmode=disable",
		"AWS_REGION":            "us-east-1",
		"AWS_ENDPOINT_URL":      "http://localhost:4566",
		"SQS_INPUT_QUEUE_NAME":  "wager-transactions.fifo",
		"SQS_INPUT_DLQ_NAME":    "wager-transactions-dlq.fifo",
		"SQS_EVENTS_QUEUE_NAME": "wallet-events.fifo",
		"OIDC_ISSUER_URL":       "http://localhost:8180/realms/wager",
		"OIDC_JWKS_URL":         "http://localhost:8180/realms/wager/protocol/openid-connect/certs",
		"OIDC_AUDIENCE":         "wager-wallet-service",
	}
	cfg, err := config.Load(func(key string) (string, bool) {
		if v, ok := overrides[key]; ok {
			return v, true
		}
		if key == "APP_HTTP_ADDR" || key == "APP_INSTANCE_ID" {
			return defaults[key], true
		}
		if v := env(key, ""); v != "" {
			return v, true
		}
		v, ok := defaults[key]
		return v, ok
	})
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

var quietLogger = fx.Decorate(func() *slog.Logger { return slog.New(slog.DiscardHandler) })

// TestAppStartsAndStops sobe a composição Fx real (HTTP, consumidor SQS e
// publisher da outbox), confere que HTTP e banco respondem e que, depois do
// Stop, os workers terminaram (o Stop falha se não terminarem no prazo) e a
// porta e o pool foram liberados.
func TestAppStartsAndStops(t *testing.T) {
	var srv *httpapi.Server
	var pool *pgxpool.Pool
	app := fxtest.New(t, bootstrap.Options(appConfig(t, nil)), quietLogger, fx.Populate(&srv, &pool))

	app.RequireStart()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	addr := srv.Addr().String()
	if err := httpapi.Probe(ctx, addr); err != nil {
		t.Errorf("HTTP com a aplicação no ar: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		t.Errorf("banco com a aplicação no ar: %v", err)
	}

	app.RequireStop()

	if err := httpapi.Probe(ctx, addr); err == nil {
		t.Error("HTTP respondeu depois do Stop")
	}
	if err := pool.Ping(ctx); err == nil {
		t.Error("pool continuou aberto depois do Stop")
	}
}

// TestAppFailsToStartWithoutDatabase: a dependência é validada na
// inicialização, não no primeiro uso.
func TestAppFailsToStartWithoutDatabase(t *testing.T) {
	cfg := appConfig(t, map[string]string{
		"DATABASE_URL": "postgres://app_runtime:app_runtime@127.0.0.1:1/wager_wallet?sslmode=disable&connect_timeout=1",
	})
	app := fx.New(bootstrap.Options(cfg), quietLogger, fx.StartTimeout(5*time.Second))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := app.Start(ctx); err == nil {
		_ = app.Stop(ctx)
		t.Fatal("a aplicação iniciou sem banco")
	}
}
