package config

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

// validEnv devolve o mínimo de variáveis para uma configuração válida.
func validEnv() map[string]string {
	return map[string]string{
		"APP_INSTANCE_ID":       "test-1",
		"DATABASE_URL":          "postgres://app_runtime:x@localhost:5432/wager_wallet",
		"AWS_REGION":            "us-east-1",
		"SQS_INPUT_QUEUE_NAME":  "wager-transactions.fifo",
		"SQS_INPUT_DLQ_NAME":    "wager-transactions-dlq.fifo",
		"SQS_EVENTS_QUEUE_NAME": "wallet-events.fifo",
		"OIDC_ISSUER_URL":       "http://localhost:8180/realms/wager",
		"OIDC_JWKS_URL":         "http://keycloak:8080/realms/wager/protocol/openid-connect/certs",
		"OIDC_AUDIENCE":         "wager-wallet-service",
	}
}

func lookupFrom(env map[string]string) LookupFunc {
	return func(key string) (string, bool) {
		v, ok := env[key]
		return v, ok
	}
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(lookupFrom(validEnv()))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.HTTPAddr != ":8081" {
		t.Errorf("HTTPAddr = %q, want :8081", cfg.HTTPAddr)
	}
	if cfg.ShutdownTimeout != 20*time.Second {
		t.Errorf("ShutdownTimeout = %v, want 20s", cfg.ShutdownTimeout)
	}
	if cfg.DBMaxConns != 10 {
		t.Errorf("DBMaxConns = %d, want 10", cfg.DBMaxConns)
	}
	if cfg.DBLockTimeout != 3*time.Second {
		t.Errorf("DBLockTimeout = %v, want 3s", cfg.DBLockTimeout)
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel = %v, want INFO", cfg.LogLevel)
	}
	want := Components{HTTP: true, SQSConsumer: true, OutboxPublisher: true, PendingWorker: true}
	if cfg.Components != want {
		t.Errorf("Components = %+v, want %+v", cfg.Components, want)
	}
	if cfg.AWS.EndpointURL != "" {
		t.Errorf("AWS.EndpointURL = %q, want vazio (AWS real)", cfg.AWS.EndpointURL)
	}
}

func TestLoadOverrides(t *testing.T) {
	env := validEnv()
	env["APP_HTTP_ADDR"] = ":9999"
	env["APP_SHUTDOWN_TIMEOUT"] = "5s"
	env["LOG_LEVEL"] = "debug"
	env["AWS_ENDPOINT_URL"] = "http://localhost:4566"
	env["APP_ENABLE_HTTP"] = "false"
	env["APP_ENABLE_PENDING_WORKER"] = "false"

	cfg, err := Load(lookupFrom(env))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HTTPAddr != ":9999" || cfg.ShutdownTimeout != 5*time.Second || cfg.LogLevel != slog.LevelDebug {
		t.Errorf("overrides não aplicados: %+v", cfg)
	}
	if cfg.AWS.EndpointURL != "http://localhost:4566" {
		t.Errorf("AWS.EndpointURL = %q", cfg.AWS.EndpointURL)
	}
	want := Components{HTTP: false, SQSConsumer: true, OutboxPublisher: true, PendingWorker: false}
	if cfg.Components != want {
		t.Errorf("Components = %+v, want %+v", cfg.Components, want)
	}
}

func TestLoadInstanceIDFallsBackToHostname(t *testing.T) {
	env := validEnv()
	delete(env, "APP_INSTANCE_ID")

	cfg, err := Load(lookupFrom(env))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.InstanceID == "" {
		t.Error("InstanceID vazio; esperado o hostname")
	}
}

func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name    string
		change  func(env map[string]string)
		wantMsg string
	}{
		{"obrigatória ausente", func(e map[string]string) { delete(e, "DATABASE_URL") }, "DATABASE_URL é obrigatória"},
		{"obrigatória vazia", func(e map[string]string) { e["OIDC_JWKS_URL"] = "" }, "OIDC_JWKS_URL é obrigatória"},
		{"duração inválida", func(e map[string]string) { e["DB_LOCK_TIMEOUT"] = "3" }, "DB_LOCK_TIMEOUT inválida"},
		{"inteiro inválido", func(e map[string]string) { e["DB_MAX_CONNS"] = "dez" }, "DB_MAX_CONNS inválida"},
		{"conexões fora do intervalo", func(e map[string]string) { e["DB_MAX_CONNS"] = "0" }, "DB_MAX_CONNS deve estar entre 1 e 1000"},
		{"duração zero", func(e map[string]string) { e["APP_SHUTDOWN_TIMEOUT"] = "0s" }, "APP_SHUTDOWN_TIMEOUT deve ser maior que zero"},
		{"booleano inválido", func(e map[string]string) { e["APP_ENABLE_HTTP"] = "sim" }, "APP_ENABLE_HTTP inválida"},
		{"nível de log inválido", func(e map[string]string) { e["LOG_LEVEL"] = "verbose" }, "LOG_LEVEL inválido"},
		{"nenhum componente", func(e map[string]string) {
			e["APP_ENABLE_HTTP"] = "false"
			e["APP_ENABLE_SQS_CONSUMER"] = "false"
			e["APP_ENABLE_OUTBOX_PUBLISHER"] = "false"
			e["APP_ENABLE_PENDING_WORKER"] = "false"
		}, "pelo menos um componente"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := validEnv()
			tt.change(env)

			_, err := Load(lookupFrom(env))
			if err == nil {
				t.Fatal("Load: esperado erro, veio nil")
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("erro = %q, want conter %q", err, tt.wantMsg)
			}
		})
	}
}

func TestLoadReportsAllErrors(t *testing.T) {
	env := validEnv()
	delete(env, "DATABASE_URL")
	delete(env, "AWS_REGION")
	env["LOG_LEVEL"] = "verbose"

	_, err := Load(lookupFrom(env))
	if err == nil {
		t.Fatal("Load: esperado erro, veio nil")
	}
	for _, want := range []string{"DATABASE_URL", "AWS_REGION", "LOG_LEVEL"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("erro não menciona %s: %q", want, err)
		}
	}
}
