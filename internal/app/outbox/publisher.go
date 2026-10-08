package outbox

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/gabrielmilano/wager-wallet-service/internal/app/port"
	"github.com/gabrielmilano/wager-wallet-service/internal/app/store"
)

// Sender publica um evento no destino (SQS FIFO). Deve usar o eventId como
// chave de deduplicação: republicar o mesmo evento é seguro.
type Sender interface {
	Send(ctx context.Context, e store.OutboxEvent) error
}

// Config controla o publisher.
type Config struct {
	Owner        string        // identifica a instância no lease (locked_by)
	BatchSize    int           // eventos por reserva
	Lease        time.Duration // posse de um lote; vencida, outra instância assume
	PollInterval time.Duration // espera quando não há eventos
	MaxBackoff   time.Duration // teto da espera entre tentativas de um evento
}

func (c Config) withDefaults() Config {
	if c.BatchSize <= 0 {
		c.BatchSize = 50
	}
	if c.Lease <= 0 {
		c.Lease = 30 * time.Second
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 500 * time.Millisecond
	}
	if c.MaxBackoff <= 0 {
		c.MaxBackoff = 5 * time.Minute
	}
	return c
}

// Publisher publica os eventos da outbox depois do commit que os gravou.
// Vários publishers podem rodar ao mesmo tempo: a reserva usa SKIP LOCKED e
// lease, então cada lote tem um dono por vez e o lote de quem caiu volta à
// fila quando o lease vence.
type Publisher struct {
	tx     store.TxRunner
	sender Sender
	clock  port.Clock
	log    *slog.Logger
	cfg    Config
}

func NewPublisher(tx store.TxRunner, sender Sender, clock port.Clock, log *slog.Logger, cfg Config) *Publisher {
	return &Publisher{tx: tx, sender: sender, clock: clock, log: log, cfg: cfg.withDefaults()}
}

// Backoff devolve a espera antes da próxima tentativa: 1 s, 2 s, 4 s...
// até MaxBackoff.
func Backoff(attempts int, max time.Duration) time.Duration {
	d := time.Second
	for i := 1; i < attempts && d < max; i++ {
		d *= 2
	}
	return min(d, max)
}

// PublishBatch reserva um lote e publica. Devolve quantos foram publicados.
//
// Se um evento falhar, os seguintes do mesmo agregado (carteira) no lote não
// são enviados, para não inverter a ordem do grupo FIFO; voltam à fila.
func (p *Publisher) PublishBatch(ctx context.Context) (int, error) {
	now := p.clock.Now()
	var claimed []store.OutboxEvent
	err := p.tx.WithinTx(ctx, func(ctx context.Context, r store.Repos) error {
		var err error
		claimed, err = r.Outbox.Claim(ctx, p.cfg.Owner, now, now.Add(p.cfg.Lease), p.cfg.BatchSize)
		return err
	})
	if err != nil || len(claimed) == 0 {
		return 0, err
	}

	published := 0
	// blocked guarda, por agregado, quando o evento que falhou será tentado
	// de novo: os seguintes esperam o mesmo tempo, para não passarem à frente.
	blocked := map[uuid.UUID]time.Time{}
	for i, e := range claimed {
		if ctx.Err() != nil {
			// Encerrando: libera o resto do lote já, sem esperar o lease vencer.
			releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			for _, rest := range claimed[i:] {
				p.release(releaseCtx, rest, p.clock.Now(), "publisher encerrado")
			}
			cancel()
			break
		}
		if next, ok := blocked[e.Envelope.AggregateID]; ok {
			p.release(ctx, e, next, "evento anterior do mesmo agregado falhou")
			continue
		}
		if err := p.sender.Send(ctx, e); err != nil {
			next := p.clock.Now().Add(Backoff(e.Attempts, p.cfg.MaxBackoff))
			blocked[e.Envelope.AggregateID] = next
			p.log.Warn("falha ao publicar evento", slog.String("eventId", e.Envelope.EventID.String()),
				slog.String("eventType", e.Envelope.EventType), slog.Int("attempts", e.Attempts), slog.Any("error", err))
			p.release(ctx, e, next, err.Error())
			continue
		}
		if err := p.markPublished(ctx, e); err != nil {
			// Publicado mas não confirmado: o lease vence e o evento é
			// republicado com o mesmo eventId (deduplicado no destino).
			p.log.Warn("evento publicado sem confirmação na outbox", slog.String("eventId", e.Envelope.EventID.String()), slog.Any("error", err))
			continue
		}
		published++
	}
	return published, nil
}

func (p *Publisher) markPublished(ctx context.Context, e store.OutboxEvent) error {
	return p.tx.WithinTx(ctx, func(ctx context.Context, r store.Repos) error {
		return r.Outbox.MarkPublished(ctx, e.Envelope.EventID, p.cfg.Owner, p.clock.Now())
	})
}

func (p *Publisher) release(ctx context.Context, e store.OutboxEvent, next time.Time, reason string) {
	err := p.tx.WithinTx(ctx, func(ctx context.Context, r store.Repos) error {
		return r.Outbox.MarkFailed(ctx, e.Envelope.EventID, p.cfg.Owner, next, reason)
	})
	if err != nil {
		p.log.Warn("não foi possível liberar evento; o lease vai expirar", slog.String("eventId", e.Envelope.EventID.String()), slog.Any("error", err))
	}
}

// Run publica em laço até ctx ser cancelado. Sem eventos ou com erro de
// banco, espera PollInterval (com recuo em erros consecutivos).
func (p *Publisher) Run(ctx context.Context) {
	failures := 0
	for ctx.Err() == nil {
		n, err := p.PublishBatch(ctx)
		wait := time.Duration(0)
		switch {
		case err != nil:
			failures++
			wait = min(p.cfg.PollInterval*time.Duration(1<<min(failures, 6)), 30*time.Second)
			p.log.Warn("publisher da outbox: falha ao reservar eventos", slog.Any("error", err))
		case n == 0:
			failures = 0
			wait = p.cfg.PollInterval
		default:
			failures = 0
		}
		if wait > 0 {
			select {
			case <-ctx.Done():
			case <-time.After(wait):
			}
		}
	}
}
