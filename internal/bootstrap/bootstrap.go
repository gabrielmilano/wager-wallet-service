package bootstrap

import (
	"context"
	"log/slog"
	"net/http"
	"os"

	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"

	"github.com/gabrielmilano/wager-wallet-service/internal/adapter/httpapi"
	"github.com/gabrielmilano/wager-wallet-service/internal/platform/config"
	"github.com/gabrielmilano/wager-wallet-service/internal/platform/logging"
)

// Options monta a aplicação a partir da configuração já validada. Componentes
// desligados em cfg.Components não entram no grafo do Fx (ADR 0002).
func Options(cfg config.Config) fx.Option {
	opts := []fx.Option{
		fx.Supply(cfg),
		fx.Provide(newLogger),
		fx.WithLogger(newFxLogger),
		fx.StopTimeout(cfg.ShutdownTimeout),
		fx.Invoke(logLifecycle),
	}
	if cfg.Components.HTTP {
		opts = append(opts, httpModule)
	}
	return fx.Options(opts...)
}

func newLogger(cfg config.Config) *slog.Logger {
	return logging.New(os.Stdout, cfg.LogLevel, cfg.InstanceID)
}

// newFxLogger faz o Fx registrar seus próprios eventos (provide, invoke,
// hooks) no mesmo logger JSON. Eventos normais só aparecem em debug.
func newFxLogger(log *slog.Logger) fxevent.Logger {
	l := &fxevent.SlogLogger{Logger: log.With(slog.String("component", "fx"))}
	l.UseLogLevel(slog.LevelDebug)
	return l
}

// logLifecycle registra quais componentes esta instância executa.
//
// Os invokes dos módulos (ex.: http) rodam antes dos invokes da raiz, então
// este hook é o último a iniciar e, como o Fx para na ordem inversa, o
// primeiro a parar: por isso "iniciada" depois dos componentes e
// "encerrando" antes deles.
func logLifecycle(lc fx.Lifecycle, cfg config.Config, log *slog.Logger) {
	c := cfg.Components
	attrs := []any{
		slog.Bool("http", c.HTTP),
		slog.Bool("sqsConsumer", c.SQSConsumer),
		slog.Bool("outboxPublisher", c.OutboxPublisher),
		slog.Bool("pendingWorker", c.PendingWorker),
	}
	lc.Append(fx.Hook{
		OnStart: func(context.Context) error {
			log.Info("aplicação iniciada", attrs...)
			return nil
		},
		OnStop: func(context.Context) error {
			log.Info("encerrando aplicação")
			return nil
		},
	})
}

var httpModule = fx.Module("http",
	fx.Provide(
		httpapi.NewRouter,
		newHTTPServer,
	),
	// Invoke força a construção do servidor; sem ele o Fx não criaria um
	// valor que ninguém pede, e o hook nunca seria registrado.
	fx.Invoke(func(*httpapi.Server) {}),
)

// newHTTPServer liga o Server ao ciclo de vida. Se o servidor cair depois de
// iniciado, a aplicação inteira encerra com código 1 em vez de seguir sem
// HTTP.
func newHTTPServer(lc fx.Lifecycle, sd fx.Shutdowner, cfg config.Config, h http.Handler, log *slog.Logger) *httpapi.Server {
	srv := httpapi.NewServer(cfg.HTTPAddr, h, log, func(error) {
		_ = sd.Shutdown(fx.ExitCode(1))
	})
	lc.Append(fx.StartStopHook(srv.Start, srv.Stop))
	return srv
}
