package bootstrap

import (
	"context"
	"fmt"
	"log/slog"

	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	"go.uber.org/fx"

	sqsadapter "github.com/gabrielmilano/wager-wallet-service/internal/adapter/sqs"
	"github.com/gabrielmilano/wager-wallet-service/internal/app/outbox"
	"github.com/gabrielmilano/wager-wallet-service/internal/app/port"
	"github.com/gabrielmilano/wager-wallet-service/internal/app/store"
	"github.com/gabrielmilano/wager-wallet-service/internal/app/wagering"
	"github.com/gabrielmilano/wager-wallet-service/internal/platform/config"
)

// worker roda uma função de longa duração numa goroutine. O encerramento é
// observável: stop cancela o contexto e só retorna quando a goroutine
// terminou, ou com erro se o prazo do Fx acabar antes.
type worker struct {
	name   string
	log    *slog.Logger
	cancel context.CancelFunc
	done   chan struct{}
}

func (w *worker) start(run func(ctx context.Context)) {
	ctx, cancel := context.WithCancel(context.Background())
	w.cancel, w.done = cancel, make(chan struct{})
	go func() {
		defer close(w.done)
		run(ctx)
	}()
	w.log.Info("worker iniciado", slog.String("worker", w.name))
}

func (w *worker) stop(ctx context.Context) error {
	if w.cancel == nil {
		return nil
	}
	w.cancel()
	select {
	case <-w.done:
		w.log.Info("worker encerrado", slog.String("worker", w.name))
		return nil
	case <-ctx.Done():
		return fmt.Errorf("worker %s não terminou no prazo: %w", w.name, ctx.Err())
	}
}

// sqsModule fornece o cliente SQS (sem rede na criação).
var sqsModule = fx.Module("sqs",
	fx.Provide(func(cfg config.Config) (*awssqs.Client, error) {
		return sqsadapter.NewClient(context.Background(), cfg.AWS.Region, cfg.AWS.EndpointURL)
	}),
)

// consumerModule consome wager-transactions.fifo. As filas são resolvidas
// no início: a aplicação não sobe se elas não existirem.
var consumerModule = fx.Module("sqs-consumer",
	fx.Invoke(func(lc fx.Lifecycle, client *awssqs.Client, svc *wagering.Service, cfg config.Config, log *slog.Logger) {
		w := &worker{name: "sqs-consumer", log: log}
		lc.Append(fx.Hook{
			OnStart: func(ctx context.Context) error {
				input, err := sqsadapter.QueueURL(ctx, client, cfg.AWS.InputQueueName)
				if err != nil {
					return err
				}
				dlq, err := sqsadapter.QueueURL(ctx, client, cfg.AWS.InputDLQName)
				if err != nil {
					return err
				}
				consumer := sqsadapter.NewConsumer(client, svc, log.With(slog.String("component", "sqs-consumer")),
					sqsadapter.ConsumerConfig{QueueURL: input, DLQURL: dlq})
				w.start(consumer.Run)
				return nil
			},
			OnStop: w.stop,
		})
	}),
)

// outboxModule publica a outbox em wallet-events.fifo.
var outboxModule = fx.Module("outbox-publisher",
	fx.Invoke(func(lc fx.Lifecycle, client *awssqs.Client, tx store.TxRunner, clock port.Clock, cfg config.Config, log *slog.Logger) {
		w := &worker{name: "outbox-publisher", log: log}
		lc.Append(fx.Hook{
			OnStart: func(ctx context.Context) error {
				events, err := sqsadapter.QueueURL(ctx, client, cfg.AWS.EventsQueueName)
				if err != nil {
					return err
				}
				publisher := outbox.NewPublisher(tx, sqsadapter.NewEventSender(client, events), clock,
					log.With(slog.String("component", "outbox-publisher")), outbox.Config{Owner: cfg.InstanceID})
				w.start(publisher.Run)
				return nil
			},
			OnStop: w.stop,
		})
	}),
)
