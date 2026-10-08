package sqs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/gabrielmilano/wager-wallet-service/internal/app/apperr"
	"github.com/gabrielmilano/wager-wallet-service/internal/app/wagering"
	"github.com/gabrielmilano/wager-wallet-service/internal/domain/money"
	"github.com/gabrielmilano/wager-wallet-service/internal/platform/metrics"
)

// ConsumerName identifica este consumidor na inbox.
const ConsumerName = "wager-transactions-consumer"

// MessageType é o único tipo aceito na fila de entrada.
const MessageType = "WagerTransactionRequested"

// Codes de mensagens enviadas à DLQ por erro permanente.
const codeInvalidMessage = "INVALID_MESSAGE"

// Processor é a porta do caso de uso usada pelo consumidor.
type Processor interface {
	ProcessFromMessage(ctx context.Context, cmd wagering.Command, meta wagering.Metadata, msg wagering.InboundMessage) (wagering.Result, error)
}

// ConsumerConfig controla o laço de consumo.
type ConsumerConfig struct {
	QueueURL       string
	DLQURL         string
	MaxMessages    int32         // por ReceiveMessage (1..10)
	WaitTime       time.Duration // long polling (até 20 s)
	MessageTimeout time.Duration // prazo para tratar uma mensagem
	MaxRetryDelay  time.Duration // teto do backoff de visibilidade
	Metrics        *metrics.Metrics
}

// Consumer lê wager-transactions.fifo. Para cada mensagem:
//
//	processada, rejeitada ou pendente  -> apaga (só depois do commit)
//	inválida ou erro corrigível        -> envia à DLQ e apaga
//	falha transitória ou inesperada    -> não apaga; muda a visibilidade com
//	                                      backoff; após maxReceiveCount da
//	                                      fila, o redrive leva à DLQ
type Consumer struct {
	client    *awssqs.Client
	processor Processor
	log       *slog.Logger
	cfg       ConsumerConfig
}

func NewConsumer(client *awssqs.Client, processor Processor, log *slog.Logger, cfg ConsumerConfig) *Consumer {
	if cfg.MaxMessages <= 0 || cfg.MaxMessages > 10 {
		cfg.MaxMessages = 10
	}
	if cfg.WaitTime <= 0 {
		cfg.WaitTime = 5 * time.Second
	}
	if cfg.MessageTimeout <= 0 {
		cfg.MessageTimeout = 20 * time.Second
	}
	if cfg.MaxRetryDelay <= 0 {
		cfg.MaxRetryDelay = time.Minute
	}
	return &Consumer{client: client, processor: processor, log: log, cfg: cfg}
}

// Run consome até ctx ser cancelado. O cancelamento não interrompe o long
// polling em curso (no máximo WaitTime): cancelar a requisição no meio faria
// o broker entregar a próxima mensagem a uma conexão morta, e ela só voltaria
// depois do visibility timeout. Encerrando, as mensagens recebidas e não
// iniciadas têm a visibilidade devolvida (reentrega imediata para outra
// instância) e a mensagem em andamento termina com o próprio prazo.
func (c *Consumer) Run(ctx context.Context) {
	failures := 0
	for ctx.Err() == nil {
		receiveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.cfg.WaitTime+10*time.Second)
		out, err := c.client.ReceiveMessage(receiveCtx, &awssqs.ReceiveMessageInput{
			QueueUrl:            aws.String(c.cfg.QueueURL),
			MaxNumberOfMessages: c.cfg.MaxMessages,
			WaitTimeSeconds:     int32(c.cfg.WaitTime / time.Second),
			MessageSystemAttributeNames: []types.MessageSystemAttributeName{
				types.MessageSystemAttributeNameApproximateReceiveCount,
				types.MessageSystemAttributeNameMessageGroupId,
			},
		})
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			// SQS indisponível: espera crescente e tenta de novo.
			failures++
			wait := min(time.Second*time.Duration(1<<min(failures, 5)), 30*time.Second)
			c.log.Warn("falha ao receber mensagens", slog.Any("error", err), slog.Duration("retryIn", wait))
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
			continue
		}
		failures = 0

		// Mensagens do mesmo grupo FIFO chegam em ordem; trata em sequência.
		for i, m := range out.Messages {
			if ctx.Err() != nil {
				c.release(out.Messages[i:])
				return
			}
			c.handle(m)
		}
	}
}

// envelope é o corpo da mensagem; data é decodificado em separado, de forma
// estrita.
type envelope struct {
	MessageID  string          `json:"messageId"`
	Type       string          `json:"type"`
	OccurredAt string          `json:"occurredAt"`
	Data       json.RawMessage `json:"data"`
}

type messageData struct {
	ProviderID                     string      `json:"providerId"`
	ExternalTransactionID          string      `json:"externalTransactionId"`
	IdempotencyKey                 string      `json:"idempotencyKey"`
	PlayerID                       string      `json:"playerId"`
	WalletID                       string      `json:"walletId"`
	RoundID                        string      `json:"roundId"`
	GameID                         string      `json:"gameId"`
	Kind                           string      `json:"kind"`
	Money                          money.Money `json:"money"`
	ReferenceExternalTransactionID string      `json:"referenceExternalTransactionId,omitempty"`
}

// handle trata uma mensagem com contexto próprio: o encerramento da
// aplicação não interrompe uma mensagem já iniciada.
func (c *Consumer) handle(m types.Message) {
	ctx, cancel := context.WithTimeout(context.Background(), c.cfg.MessageTimeout)
	defer cancel()

	body := aws.ToString(m.Body)
	env, data, err := parse(body)
	log := c.log.With(slog.String("sqsMessageId", aws.ToString(m.MessageId)), slog.String("messageId", env.MessageID))
	if err != nil {
		c.deadLetter(ctx, log, m, codeInvalidMessage, err.Error())
		return
	}

	cmd, err := wagering.NewCommand(wagering.Input{
		ProviderID: data.ProviderID, ExternalTransactionID: data.ExternalTransactionID,
		PlayerID: data.PlayerID, WalletID: data.WalletID, RoundID: data.RoundID, GameID: data.GameID,
		Kind: data.Kind, Money: data.Money, ReferenceExternalTransactionID: data.ReferenceExternalTransactionID,
	})
	if err != nil {
		if e, ok := apperr.As(err); ok {
			c.deadLetter(ctx, log, m, e.Code, e.Message)
		} else {
			c.retryLater(log, m, err)
		}
		return
	}

	sum := sha256.Sum256([]byte(body))
	started := time.Now()
	res, err := c.processor.ProcessFromMessage(ctx, cmd,
		wagering.Metadata{IdempotencyKey: data.IdempotencyKey, CorrelationID: env.MessageID, CausationID: env.MessageID},
		wagering.InboundMessage{ConsumerName: ConsumerName, MessageID: env.MessageID, PayloadHash: sum[:]})
	if err != nil {
		if e, ok := apperr.As(err); ok && e.Kind != apperr.Unavailable {
			// Corrigível: reentregar não muda o resultado.
			c.deadLetter(ctx, log, m, e.Code, e.Message)
			return
		}
		c.retryLater(log, m, err)
		return
	}

	tx := res.Transaction
	c.cfg.Metrics.ObserveOperation("sqs", string(tx.Kind()), string(tx.Status()), res.Replay, time.Since(started))
	c.cfg.Metrics.SQSMessage("processed")
	log.Info("mensagem processada",
		slog.String("transactionId", tx.ID().String()), slog.String("walletId", tx.WalletID().String()),
		slog.String("providerId", tx.ProviderID()), slog.String("status", string(tx.Status())),
		slog.String("failureCode", string(tx.FailureCode())), slog.Bool("idempotentReplay", res.Replay))
	// Só depois do commit do tratamento durável.
	c.delete(ctx, log, m)
}

func parse(body string) (envelope, messageData, error) {
	var env envelope
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		return env, messageData{}, fmt.Errorf("envelope inválido: %v", err)
	}
	switch {
	case env.MessageID == "":
		return env, messageData{}, errors.New("messageId ausente")
	case env.Type != MessageType:
		return env, messageData{}, fmt.Errorf("type %q não suportado", env.Type)
	case len(env.Data) == 0:
		return env, messageData{}, errors.New("data ausente")
	}
	var data messageData
	dec := json.NewDecoder(bytes.NewReader(env.Data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&data); err != nil {
		return env, messageData{}, fmt.Errorf("data inválido: %v", err)
	}
	return env, data, nil
}

func (c *Consumer) delete(ctx context.Context, log *slog.Logger, m types.Message) {
	_, err := c.client.DeleteMessage(ctx, &awssqs.DeleteMessageInput{
		QueueUrl: aws.String(c.cfg.QueueURL), ReceiptHandle: m.ReceiptHandle,
	})
	if err != nil {
		// A mensagem volta após o visibility timeout e a inbox a reconhece.
		log.Warn("falha ao apagar mensagem já processada", slog.Any("error", err))
	}
}

// deadLetter envia a mensagem à DLQ com o motivo e a apaga da entrada. Se
// cair entre os dois passos, a reentrega chega ao mesmo erro e o SQS
// deduplica o reenvio à DLQ (MessageDeduplicationId = id da mensagem SQS).
func (c *Consumer) deadLetter(ctx context.Context, log *slog.Logger, m types.Message, code, reason string) {
	log.Warn("mensagem enviada à DLQ", slog.String("code", code), slog.String("reason", reason))
	c.cfg.Metrics.SQSMessage("dead_letter")
	group := m.Attributes[string(types.MessageSystemAttributeNameMessageGroupId)]
	if group == "" {
		group = "dlq"
	}
	_, err := c.client.SendMessage(ctx, &awssqs.SendMessageInput{
		QueueUrl:               aws.String(c.cfg.DLQURL),
		MessageBody:            m.Body,
		MessageGroupId:         aws.String(group),
		MessageDeduplicationId: m.MessageId,
		MessageAttributes: map[string]types.MessageAttributeValue{
			"failureCode":   {DataType: aws.String("String"), StringValue: aws.String(code)},
			"failureReason": {DataType: aws.String("String"), StringValue: aws.String(truncate(reason, 256))},
		},
	})
	if err != nil {
		c.retryLater(log, m, fmt.Errorf("enviar à DLQ: %w", err))
		return
	}
	c.delete(ctx, log, m)
}

// retryLater deixa a mensagem na fila e adia a reentrega com backoff pelo
// número de recebimentos (2 s, 4 s, 8 s... até MaxRetryDelay).
func (c *Consumer) retryLater(log *slog.Logger, m types.Message, cause error) {
	receives, _ := strconv.Atoi(m.Attributes[string(types.MessageSystemAttributeNameApproximateReceiveCount)])
	delay := min(time.Second*time.Duration(1<<min(max(receives, 1), 10)), c.cfg.MaxRetryDelay)
	c.cfg.Metrics.SQSMessage("retry")
	if apperr.IsKind(cause, apperr.Unavailable) {
		c.cfg.Metrics.Conflict(conflictReason(cause))
	}
	log.Warn("falha transitória; mensagem será reentregue", slog.Any("error", cause),
		slog.Int("receiveCount", receives), slog.Duration("retryIn", delay))
	c.setVisibility(m, delay)
}

// release devolve mensagens recebidas e não iniciadas (encerramento).
func (c *Consumer) release(msgs []types.Message) {
	for _, m := range msgs {
		c.cfg.Metrics.SQSMessage("released")
		c.setVisibility(m, 0)
	}
}

func (c *Consumer) setVisibility(m types.Message, d time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := c.client.ChangeMessageVisibility(ctx, &awssqs.ChangeMessageVisibilityInput{
		QueueUrl: aws.String(c.cfg.QueueURL), ReceiptHandle: m.ReceiptHandle, VisibilityTimeout: int32(d / time.Second),
	})
	if err != nil {
		c.log.Warn("falha ao mudar visibilidade; vale o visibility timeout da fila", slog.Any("error", err))
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func conflictReason(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "55P03"):
		return "lock_timeout"
	case strings.Contains(msg, "40P01"):
		return "deadlock"
	default:
		return "unavailable"
	}
}
