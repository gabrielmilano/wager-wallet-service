//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/google/uuid"

	"github.com/gabrielmilano/wager-wallet-service/internal/adapter/postgres"
	sqsadapter "github.com/gabrielmilano/wager-wallet-service/internal/adapter/sqs"
	"github.com/gabrielmilano/wager-wallet-service/internal/app/outbox"
	"github.com/gabrielmilano/wager-wallet-service/internal/app/port"
	"github.com/gabrielmilano/wager-wallet-service/internal/app/store"
	"github.com/gabrielmilano/wager-wallet-service/internal/domain/event"
	"github.com/gabrielmilano/wager-wallet-service/migrations"
)

// --- consumidor (app do Compose) ---------------------------------------------

func inputQueueURL(t *testing.T, ctx context.Context, c *awssqs.Client, name string) string {
	t.Helper()
	u, err := c.GetQueueUrl(ctx, &awssqs.GetQueueUrlInput{QueueName: aws.String(name)})
	if err != nil {
		t.Fatal(err)
	}
	return aws.ToString(u.QueueUrl)
}

// wagerMessage monta o envelope do enunciado.
func wagerMessage(messageID string, data map[string]any) string {
	body, _ := json.Marshal(map[string]any{
		"messageId": messageID, "type": "WagerTransactionRequested",
		"occurredAt": "2026-10-07T12:00:00.000Z", "data": data,
	})
	return string(body)
}

func sqsData(op map[string]any) map[string]any {
	data := map[string]any{}
	for k, v := range op {
		data[k] = v
	}
	data["idempotencyKey"] = "provider-a:" + op["externalTransactionId"].(string)
	return data
}

// send publica na fila de entrada. dedupID diferente faz o SQS entregar de
// novo o mesmo corpo: é assim que os testes forçam a reentrega.
func send(t *testing.T, ctx context.Context, c *awssqs.Client, queueURL, group, dedupID, body string) {
	t.Helper()
	_, err := c.SendMessage(ctx, &awssqs.SendMessageInput{
		QueueUrl: aws.String(queueURL), MessageBody: aws.String(body),
		MessageGroupId: aws.String(group), MessageDeduplicationId: aws.String(dedupID),
	})
	if err != nil {
		t.Fatal(err)
	}
}

// eventually repete check até ele devolver true ou o prazo acabar.
func eventually(t *testing.T, timeout time.Duration, what string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("tempo esgotado esperando: %s", what)
}

func TestSQSConsumerProcessesAndDeduplicates(t *testing.T) {
	ctx := testContext(t)
	c := sqsClient(t, ctx)
	u := newUsecases(t, ctx, 3*time.Second)
	queue := inputQueueURL(t, ctx, c, env("SQS_INPUT_QUEUE_NAME", "wager-transactions.fifo"))

	walletID, playerID := openWalletHTTP(t, "100.00")
	op := operation(walletID, playerID, "BET", "25.00", "")
	messageID := "msg-" + uuid.NewString()
	body := wagerMessage(messageID, sqsData(op))

	// A mesma mensagem entregue duas vezes (dedup do SQS contornado de
	// propósito): quem deduplica é a inbox da aplicação.
	send(t, ctx, c, queue, walletID, "a-"+messageID, body)
	send(t, ctx, c, queue, walletID, "b-"+messageID, body)

	eventually(t, 15*time.Second, "operação processada pelo consumidor", func() bool {
		return u.count(t, ctx, `SELECT count(*) FROM inbox_messages WHERE message_id = $1 AND processed_at IS NOT NULL`, messageID) == 1 &&
			walletBalance(t, walletID) == "75.00"
	})
	// Dá tempo de a segunda entrega ser tratada e conferida.
	eventually(t, 15*time.Second, "fila de entrada vazia", func() bool {
		out, err := c.GetQueueAttributes(ctx, &awssqs.GetQueueAttributesInput{
			QueueUrl: aws.String(queue),
			AttributeNames: []sqstypes.QueueAttributeName{
				sqstypes.QueueAttributeNameApproximateNumberOfMessages,
				sqstypes.QueueAttributeNameApproximateNumberOfMessagesNotVisible,
			},
		})
		return err == nil && out.Attributes["ApproximateNumberOfMessages"] == "0" &&
			out.Attributes["ApproximateNumberOfMessagesNotVisible"] == "0"
	})

	if n := u.count(t, ctx, `SELECT count(*) FROM wager_transactions WHERE external_transaction_id = $1`,
		op["externalTransactionId"]); n != 1 {
		t.Errorf("operações = %d, want 1", n)
	}
	if got := walletBalance(t, walletID); got != "75.00" {
		t.Errorf("saldo = %s, want 75.00 (um único débito)", got)
	}
}

func TestSQSPermanentErrorsGoToDLQ(t *testing.T) {
	ctx := testContext(t)
	c := sqsClient(t, ctx)
	queue := inputQueueURL(t, ctx, c, env("SQS_INPUT_QUEUE_NAME", "wager-transactions.fifo"))
	dlq := inputQueueURL(t, ctx, c, env("SQS_INPUT_DLQ_NAME", "wager-transactions-dlq.fifo"))

	_, playerID := openWalletHTTP(t, "10.00")
	marker := uuid.NewString()
	invalid := `{"nao":"e o envelope","marker":"` + marker + `"}`
	unknownWallet := wagerMessage("msg-"+marker, sqsData(operation(uuid.NewString(), playerID, "BET", "1.00", "")))

	send(t, ctx, c, queue, "g-"+marker, "inv-"+marker, invalid)
	send(t, ctx, c, queue, "g-"+marker, "wal-"+marker, unknownWallet)

	want := map[string]string{"INVALID_MESSAGE": invalid, "WALLET_NOT_FOUND": unknownWallet}
	found := map[string]bool{}
	eventually(t, 20*time.Second, "mensagens na DLQ", func() bool {
		out, err := c.ReceiveMessage(ctx, &awssqs.ReceiveMessageInput{
			QueueUrl: aws.String(dlq), MaxNumberOfMessages: 10, WaitTimeSeconds: 1,
			MessageAttributeNames: []string{"All"},
		})
		if err != nil {
			return false
		}
		for _, m := range out.Messages {
			if !strings.Contains(aws.ToString(m.Body), marker) {
				continue
			}
			code := aws.ToString(m.MessageAttributes["failureCode"].StringValue)
			if want[code] == aws.ToString(m.Body) {
				found[code] = true
			}
			_, _ = c.DeleteMessage(ctx, &awssqs.DeleteMessageInput{QueueUrl: aws.String(dlq), ReceiptHandle: m.ReceiptHandle})
		}
		return len(found) == len(want)
	})
}

func TestHTTPAndSQSSameOperationMoveOnce(t *testing.T) {
	ctx := testContext(t)
	c := sqsClient(t, ctx)
	u := newUsecases(t, ctx, 3*time.Second)
	queue := inputQueueURL(t, ctx, c, env("SQS_INPUT_QUEUE_NAME", "wager-transactions.fifo"))

	walletID, playerID := openWalletHTTP(t, "100.00")
	op := operation(walletID, playerID, "BET", "40.00", "")
	messageID := "msg-" + uuid.NewString()

	// Ao mesmo tempo: a mesma operação por SQS e por HTTP.
	var wg sync.WaitGroup
	var httpResp apiResponse
	wg.Add(2)
	go func() {
		defer wg.Done()
		send(t, ctx, c, queue, walletID, messageID, wagerMessage(messageID, sqsData(op)))
	}()
	go func() { defer wg.Done(); httpResp = postOperation(t, token(t, "provider-a"), op) }()
	wg.Wait()

	if httpResp.status != 200 {
		t.Fatalf("HTTP = %d %s", httpResp.status, httpResp.rawBody)
	}
	eventually(t, 15*time.Second, "mensagem SQS tratada", func() bool {
		return u.count(t, ctx, `SELECT count(*) FROM inbox_messages WHERE message_id = $1 AND processed_at IS NOT NULL`, messageID) == 1
	})
	var inboxTx string
	if err := u.pool.QueryRow(ctx, `SELECT transaction_id::text FROM inbox_messages WHERE message_id = $1`, messageID).Scan(&inboxTx); err != nil {
		t.Fatal(err)
	}
	if inboxTx != httpResp.str("transactionId") {
		t.Errorf("HTTP e SQS apontam para operações diferentes: %s e %s", httpResp.str("transactionId"), inboxTx)
	}
	if got := walletBalance(t, walletID); got != "60.00" {
		t.Errorf("saldo = %s, want 60.00 (um único débito)", got)
	}
}

// --- publisher da outbox (banco e fila temporários) ---------------------------

// newOutboxEnv isola o publisher do que roda no Compose: banco temporário
// migrado e fila FIFO temporária. Devolve o TxRunner, o cliente SQS, a URL da
// fila e a URL do banco como app_runtime.
func newOutboxEnv(t *testing.T, ctx context.Context) (store.TxRunner, *awssqs.Client, string, string) {
	t.Helper()
	migratorURL := tempDatabase(t, ctx)
	m, err := postgres.NewMigrator(migrations.FS, migratorURL)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Up(); err != nil {
		t.Fatal(err)
	}
	_ = m.Close()

	parsed, _ := url.Parse(migratorURL)
	runtimeURL := withDatabase(t, env("DATABASE_URL",
		"postgres://app_runtime:app_runtime@localhost:5432/wager_wallet?sslmode=disable"), strings.TrimPrefix(parsed.Path, "/"))
	pool, err := postgres.NewPool(ctx, runtimeURL, 10)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	c := sqsClient(t, ctx)
	q, err := c.CreateQueue(ctx, &awssqs.CreateQueueInput{
		QueueName:  aws.String(fmt.Sprintf("events-probe-%d.fifo", time.Now().UnixNano())),
		Attributes: map[string]string{"FifoQueue": "true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = c.DeleteQueue(context.Background(), &awssqs.DeleteQueueInput{QueueUrl: q.QueueUrl}) })
	return postgres.NewTxRunner(pool, 3*time.Second), c, aws.ToString(q.QueueUrl), runtimeURL
}

// insertEvents grava n eventos de uma carteira na outbox.
func insertEvents(t *testing.T, ctx context.Context, tx store.TxRunner, aggregate uuid.UUID, n int) []uuid.UUID {
	t.Helper()
	var ids []uuid.UUID
	err := tx.WithinTx(ctx, func(ctx context.Context, r store.Repos) error {
		for i := range n {
			id := port.UUIDv7{}.NewID()
			ids = append(ids, id)
			if err := r.Outbox.Insert(ctx, event.Envelope{
				EventID: id, EventType: event.TypeWalletBalanceChanged, AggregateID: aggregate,
				CorrelationID: "corr", OccurredAt: time.Now().UTC(), Version: 1, Data: map[string]int{"seq": i},
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

// countingSender conta envios por eventId, para detectar publicação dupla
// que a deduplicação do SQS esconderia.
type countingSender struct {
	next outbox.Sender
	mu   sync.Mutex
	sent map[uuid.UUID]int
	fail bool
}

func (s *countingSender) Send(ctx context.Context, e store.OutboxEvent) error {
	if s.fail {
		return errors.New("destino indisponível (simulado)")
	}
	s.mu.Lock()
	s.sent[e.Envelope.EventID]++
	s.mu.Unlock()
	return s.next.Send(ctx, e)
}

func publisher(tx store.TxRunner, sender outbox.Sender, owner string, lease time.Duration) *outbox.Publisher {
	return outbox.NewPublisher(tx, sender, port.SystemClock{}, slog.New(slog.DiscardHandler),
		outbox.Config{Owner: owner, BatchSize: 7, Lease: lease, PollInterval: 20 * time.Millisecond})
}

func unpublished(t *testing.T, ctx context.Context, tx store.TxRunner) int {
	t.Helper()
	n := 0
	err := tx.WithinTx(ctx, func(ctx context.Context, r store.Repos) error {
		claimed, err := r.Outbox.Claim(ctx, "contador", time.Now().Add(time.Hour), time.Now(), 1000)
		n = len(claimed)
		if n > 0 {
			return errors.New("rollback: só contando")
		}
		return err
	})
	if err != nil && n == 0 {
		t.Fatal(err)
	}
	return n
}

func TestOutboxConcurrentPublishers(t *testing.T) {
	ctx := testContext(t)
	tx, c, queue, _ := newOutboxEnv(t, ctx)
	ids := insertEvents(t, ctx, tx, uuid.New(), 30)
	ids = append(ids, insertEvents(t, ctx, tx, uuid.New(), 30)...)

	sender := &countingSender{next: sqsadapter.NewEventSender(c, queue), sent: map[uuid.UUID]int{}}
	var wg sync.WaitGroup
	for _, owner := range []string{"instancia-1", "instancia-2"} {
		p := publisher(tx, sender, owner, 30*time.Second)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				if n, err := p.PublishBatch(ctx); err != nil {
					t.Errorf("%s: %v", owner, err)
					return
				} else if n == 0 {
					return
				}
			}
		}()
	}
	wg.Wait()

	for _, id := range ids {
		if sender.sent[id] != 1 {
			t.Errorf("evento %s enviado %d vezes, want 1", id, sender.sent[id])
		}
	}
	if n := unpublished(t, ctx, tx); n != 0 {
		t.Errorf("eventos não publicados: %d", n)
	}
	if got := countQueue(t, ctx, c, queue); got != len(ids) {
		t.Errorf("mensagens na fila = %d, want %d", got, len(ids))
	}
}

func TestOutboxLeaseRecoveryKeepsEventID(t *testing.T) {
	ctx := testContext(t)
	tx, c, queue, _ := newOutboxEnv(t, ctx)
	ids := insertEvents(t, ctx, tx, uuid.New(), 3)

	// Uma instância reserva o lote e "cai" antes de publicar.
	err := tx.WithinTx(ctx, func(ctx context.Context, r store.Repos) error {
		_, err := r.Outbox.Claim(ctx, "instancia-que-caiu", time.Now(), time.Now().Add(500*time.Millisecond), 10)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	sender := &countingSender{next: sqsadapter.NewEventSender(c, queue), sent: map[uuid.UUID]int{}}
	p := publisher(tx, sender, "instancia-viva", 30*time.Second)
	if n, _ := p.PublishBatch(ctx); n != 0 {
		t.Fatalf("publicou %d eventos com o lease ainda válido", n)
	}
	time.Sleep(600 * time.Millisecond)
	if n, err := p.PublishBatch(ctx); err != nil || n != 3 {
		t.Fatalf("depois do lease: publicou %d, %v", n, err)
	}
	for _, id := range ids {
		if sender.sent[id] != 1 {
			t.Errorf("evento %s não foi assumido pela outra instância", id)
		}
	}
	got := receiveEventIDs(t, ctx, c, queue)
	for _, id := range ids {
		if !got[id.String()] {
			t.Errorf("eventId %s não chegou à fila (republicação deve preservar o eventId)", id)
		}
	}
}

func TestOutboxRepublishAfterCrashIsDeduplicated(t *testing.T) {
	ctx := testContext(t)
	tx, c, queue, _ := newOutboxEnv(t, ctx)
	insertEvents(t, ctx, tx, uuid.New(), 1)

	// Instância A publica, mas cai antes de confirmar na outbox.
	var claimed []store.OutboxEvent
	err := tx.WithinTx(ctx, func(ctx context.Context, r store.Repos) error {
		var err error
		claimed, err = r.Outbox.Claim(ctx, "instancia-a", time.Now(), time.Now().Add(300*time.Millisecond), 10)
		return err
	})
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: %v %d", err, len(claimed))
	}
	if err := sqsadapter.NewEventSender(c, queue).Send(ctx, claimed[0]); err != nil {
		t.Fatal(err)
	}

	// Instância B assume depois do lease e publica de novo, com o mesmo eventId.
	time.Sleep(400 * time.Millisecond)
	p := publisher(tx, sqsadapter.NewEventSender(c, queue), "instancia-b", 30*time.Second)
	if n, err := p.PublishBatch(ctx); err != nil || n != 1 {
		t.Fatalf("republicação: %d, %v", n, err)
	}
	if got := countQueue(t, ctx, c, queue); got != 1 {
		t.Errorf("mensagens na fila = %d, want 1 (MessageDeduplicationId = eventId)", got)
	}
}

func TestOutboxSendFailureBacksOffAndKeepsOrder(t *testing.T) {
	ctx := testContext(t)
	tx, c, queue, runtimeURL := newOutboxEnv(t, ctx)
	ids := insertEvents(t, ctx, tx, uuid.New(), 3)

	failing := &countingSender{next: sqsadapter.NewEventSender(c, queue), sent: map[uuid.UUID]int{}, fail: true}
	if n, err := publisher(tx, failing, "instancia-1", 30*time.Second).PublishBatch(ctx); err != nil || n != 0 {
		t.Fatalf("PublishBatch com destino fora: %d, %v", n, err)
	}

	conn := connect(t, ctx, runtimeURL)
	var attempts int
	var nextAttempt time.Time
	var lastError *string
	var lockedBy *string
	err := conn.QueryRow(ctx, `SELECT attempts, next_attempt_at, last_error, locked_by FROM outbox_events WHERE event_id = $1`,
		ids[0]).Scan(&attempts, &nextAttempt, &lastError, &lockedBy)
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 1 || !nextAttempt.After(time.Now()) || lastError == nil || lockedBy != nil {
		t.Errorf("primeiro evento: tentativas %d, próxima %v, erro %v, lease %v", attempts, nextAttempt, lastError, lockedBy)
	}
	// Os seguintes do mesmo agregado não foram enviados e esperam o mesmo
	// tempo do que falhou (não passam à frente dele no grupo FIFO).
	for _, id := range ids[1:] {
		if failing.sent[id] != 0 {
			t.Errorf("evento %s enviado depois de uma falha anterior do mesmo agregado", id)
		}
		var next time.Time
		if err := conn.QueryRow(ctx, `SELECT next_attempt_at FROM outbox_events WHERE event_id = $1`, id).Scan(&next); err != nil {
			t.Fatal(err)
		}
		if next.Before(nextAttempt) {
			t.Errorf("evento %s agendado para %v, antes do evento que falhou (%v)", id, next, nextAttempt)
		}
	}
}

func countQueue(t *testing.T, ctx context.Context, c *awssqs.Client, queue string) int {
	t.Helper()
	return len(receiveEventIDs(t, ctx, c, queue))
}

// receiveEventIDs lê e apaga a fila inteira (temporária) e devolve os
// eventIds. Apagar é necessário: numa fila FIFO, enquanto há mensagens de um
// grupo em voo, o SQS não entrega as seguintes do mesmo grupo.
func receiveEventIDs(t *testing.T, ctx context.Context, c *awssqs.Client, queue string) map[string]bool {
	t.Helper()
	ids := map[string]bool{}
	for empty := 0; empty < 2; {
		out, err := c.ReceiveMessage(ctx, &awssqs.ReceiveMessageInput{
			QueueUrl: aws.String(queue), MaxNumberOfMessages: 10, WaitTimeSeconds: 1, VisibilityTimeout: 60,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(out.Messages) == 0 {
			empty++
			continue
		}
		for _, m := range out.Messages {
			var body struct {
				EventID string `json:"eventId"`
			}
			_ = json.Unmarshal([]byte(aws.ToString(m.Body)), &body)
			ids[body.EventID] = true
			_, _ = c.DeleteMessage(ctx, &awssqs.DeleteMessageInput{QueueUrl: aws.String(queue), ReceiptHandle: m.ReceiptHandle})
		}
	}
	return ids
}
