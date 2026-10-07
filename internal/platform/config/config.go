package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"
)

// Config é a configuração da aplicação, lida de variáveis de ambiente.
type Config struct {
	HTTPAddr        string
	InstanceID      string
	ShutdownTimeout time.Duration
	LogLevel        slog.Level

	DatabaseURL   string
	DBLockTimeout time.Duration

	AWS  AWS
	OIDC OIDC

	Components Components
}

// AWS agrupa a configuração do SQS. EndpointURL vazio significa AWS real.
type AWS struct {
	Region          string
	EndpointURL     string
	InputQueueName  string
	InputDLQName    string
	EventsQueueName string
}

// OIDC agrupa a validação de tokens. Não há discovery: as chaves vêm de
// JWKSURL e o iss do token é comparado com IssuerURL como texto (ADR 0009).
type OIDC struct {
	IssuerURL string
	JWKSURL   string
	Audience  string
}

// Components liga e desliga cada componente do binário (ADR 0002).
type Components struct {
	HTTP            bool
	SQSConsumer     bool
	OutboxPublisher bool
	PendingWorker   bool
}

// LookupFunc tem a assinatura de os.LookupEnv; nos testes é substituída por
// um mapa.
type LookupFunc func(key string) (string, bool)

// Load lê e valida a configuração. Todos os problemas são reunidos em um
// único erro, para que uma configuração ruim seja corrigida de uma vez.
func Load(lookup LookupFunc) (Config, error) {
	r := reader{lookup: lookup}

	cfg := Config{
		HTTPAddr:        r.str("APP_HTTP_ADDR", ":8081"),
		InstanceID:      r.str("APP_INSTANCE_ID", ""),
		ShutdownTimeout: r.duration("APP_SHUTDOWN_TIMEOUT", 20*time.Second),
		LogLevel:        r.level("LOG_LEVEL", slog.LevelInfo),

		DatabaseURL:   r.required("DATABASE_URL"),
		DBLockTimeout: r.duration("DB_LOCK_TIMEOUT", 3*time.Second),

		AWS: AWS{
			Region:          r.required("AWS_REGION"),
			EndpointURL:     r.str("AWS_ENDPOINT_URL", ""),
			InputQueueName:  r.required("SQS_INPUT_QUEUE_NAME"),
			InputDLQName:    r.required("SQS_INPUT_DLQ_NAME"),
			EventsQueueName: r.required("SQS_EVENTS_QUEUE_NAME"),
		},
		OIDC: OIDC{
			IssuerURL: r.required("OIDC_ISSUER_URL"),
			JWKSURL:   r.required("OIDC_JWKS_URL"),
			Audience:  r.required("OIDC_AUDIENCE"),
		},
		Components: Components{
			HTTP:            r.boolean("APP_ENABLE_HTTP", true),
			SQSConsumer:     r.boolean("APP_ENABLE_SQS_CONSUMER", true),
			OutboxPublisher: r.boolean("APP_ENABLE_OUTBOX_PUBLISHER", true),
			PendingWorker:   r.boolean("APP_ENABLE_PENDING_WORKER", true),
		},
	}

	if cfg.ShutdownTimeout <= 0 {
		r.fail("APP_SHUTDOWN_TIMEOUT deve ser maior que zero")
	}
	if cfg.DBLockTimeout <= 0 {
		r.fail("DB_LOCK_TIMEOUT deve ser maior que zero")
	}
	if c := cfg.Components; !c.HTTP && !c.SQSConsumer && !c.OutboxPublisher && !c.PendingWorker {
		r.fail("pelo menos um componente APP_ENABLE_* deve estar ligado")
	}
	if cfg.InstanceID == "" {
		host, err := os.Hostname()
		if err != nil {
			r.fail(fmt.Sprintf("APP_INSTANCE_ID ausente e hostname indisponível: %v", err))
		}
		cfg.InstanceID = host
	}

	if err := errors.Join(r.errs...); err != nil {
		return Config{}, fmt.Errorf("configuração inválida: %w", err)
	}
	return cfg, nil
}

// reader acumula erros de leitura em vez de parar no primeiro.
type reader struct {
	lookup LookupFunc
	errs   []error
}

func (r *reader) fail(msg string) {
	r.errs = append(r.errs, errors.New(msg))
}

func (r *reader) str(key, def string) string {
	if v, ok := r.lookup(key); ok && v != "" {
		return v
	}
	return def
}

func (r *reader) required(key string) string {
	v, ok := r.lookup(key)
	if !ok || v == "" {
		r.fail(key + " é obrigatória")
	}
	return v
}

func (r *reader) duration(key string, def time.Duration) time.Duration {
	v, ok := r.lookup(key)
	if !ok || v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		r.fail(fmt.Sprintf("%s inválida (%q): use o formato 3s, 500ms", key, v))
		return def
	}
	return d
}

func (r *reader) boolean(key string, def bool) bool {
	v, ok := r.lookup(key)
	if !ok || v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		r.fail(fmt.Sprintf("%s inválida (%q): use true ou false", key, v))
		return def
	}
	return b
}

func (r *reader) level(key string, def slog.Level) slog.Level {
	v, ok := r.lookup(key)
	if !ok || v == "" {
		return def
	}
	var l slog.Level
	if err := l.UnmarshalText([]byte(v)); err != nil {
		r.fail(fmt.Sprintf("%s inválido (%q): use debug, info, warn ou error", key, v))
		return def
	}
	return l
}
