package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics reúne as métricas do serviço num registro próprio. Todos os
// métodos aceitam receptor nil (não fazem nada), para que testes e
// componentes opcionais não precisem de métricas.
type Metrics struct {
	registry *prometheus.Registry

	operations         *prometheus.CounterVec
	replays            *prometheus.CounterVec
	processing         *prometheus.HistogramVec
	conflicts          *prometheus.CounterVec
	sqsMessages        *prometheus.CounterVec
	outboxPublished    prometheus.Counter
	outboxFailures     prometheus.Counter
	outboxPending      prometheus.Gauge
	outboxLag          prometheus.Gauge
	reconciliations    *prometheus.CounterVec
	pendingTransitions *prometheus.CounterVec
}

// New cria e registra as métricas.
func New() *Metrics {
	m := &Metrics{
		registry: prometheus.NewRegistry(),
		operations: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "wager_operations_total",
			Help: "Operações concluídas por origem, tipo e estado (PROCESSED, REJECTED, PENDING_REFERENCE, FAILED).",
		}, []string{"source", "kind", "status"}),
		replays: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "wager_idempotent_replays_total",
			Help: "Requisições ou mensagens duplicadas respondidas com o resultado gravado.",
		}, []string{"source"}),
		processing: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "wager_processing_seconds",
			Help:    "Latência de processamento de uma operação (caso de uso completo).",
			Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5},
		}, []string{"source"}),
		conflicts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "wager_conflicts_total",
			Help: "Conflitos: idempotência (409) e de concorrência (lock_timeout, deadlock).",
		}, []string{"reason"}),
		sqsMessages: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sqs_messages_total",
			Help: "Mensagens da fila de entrada por destino: processed, dead_letter, retry, released.",
		}, []string{"outcome"}),
		outboxPublished: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "outbox_events_published_total", Help: "Eventos publicados a partir da outbox.",
		}),
		outboxFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "outbox_publish_failures_total", Help: "Falhas de envio de eventos (retentados com backoff).",
		}),
		outboxPending: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "outbox_pending_events", Help: "Eventos ainda não publicados.",
		}),
		outboxLag: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "outbox_lag_seconds", Help: "Idade do evento não publicado mais antigo (atraso da outbox).",
		}),
		reconciliations: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "wallet_reconciliations_total",
			Help: "Reconciliações executadas; consistent=false indica divergência entre saldo e extrato.",
		}, []string{"consistent"}),
		pendingTransitions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "wager_pending_reference_retries_total",
			Help: "Retomadas de pendências pelo worker, por resultado.",
		}, []string{"status"}),
	}
	m.registry.MustRegister(
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.operations, m.replays, m.processing, m.conflicts, m.sqsMessages,
		m.outboxPublished, m.outboxFailures, m.outboxPending, m.outboxLag,
		m.reconciliations, m.pendingTransitions,
	)
	return m
}

// Handler expõe as métricas no formato do Prometheus.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// ObserveOperation registra o resultado de uma operação.
func (m *Metrics) ObserveOperation(source, kind, status string, replay bool, d time.Duration) {
	if m == nil {
		return
	}
	m.operations.WithLabelValues(source, kind, status).Inc()
	if replay {
		m.replays.WithLabelValues(source).Inc()
	}
	m.processing.WithLabelValues(source).Observe(d.Seconds())
}

// Conflict registra um conflito (idempotency, lock_timeout, deadlock, unavailable).
func (m *Metrics) Conflict(reason string) {
	if m == nil {
		return
	}
	m.conflicts.WithLabelValues(reason).Inc()
}

// SQSMessage registra o destino de uma mensagem de entrada.
func (m *Metrics) SQSMessage(outcome string) {
	if m == nil {
		return
	}
	m.sqsMessages.WithLabelValues(outcome).Inc()
}

func (m *Metrics) OutboxPublished(n int) {
	if m == nil {
		return
	}
	m.outboxPublished.Add(float64(n))
}

func (m *Metrics) OutboxFailed() {
	if m == nil {
		return
	}
	m.outboxFailures.Inc()
}

// OutboxBacklog registra quantos eventos faltam publicar e a idade do mais antigo.
func (m *Metrics) OutboxBacklog(pending int, oldest time.Duration) {
	if m == nil {
		return
	}
	m.outboxPending.Set(float64(pending))
	m.outboxLag.Set(oldest.Seconds())
}

// Reconciliation registra o resultado de uma reconciliação.
func (m *Metrics) Reconciliation(consistent bool) {
	if m == nil {
		return
	}
	m.reconciliations.WithLabelValues(strconv.FormatBool(consistent)).Inc()
}

// PendingResumed registra a retomada de uma pendência.
func (m *Metrics) PendingResumed(status string) {
	if m == nil {
		return
	}
	m.pendingTransitions.WithLabelValues(status).Inc()
}
