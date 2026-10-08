//go:build integration

package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/google/uuid"

	sqsadapter "github.com/gabrielmilano/wager-wallet-service/internal/adapter/sqs"
	"github.com/gabrielmilano/wager-wallet-service/internal/app/wagering"
	"github.com/gabrielmilano/wager-wallet-service/internal/domain/wager"
)

// syncBuffer é um io.Writer seguro para o log do consumidor em goroutine.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestConsumerCrashAfterCommitBeforeDelete: um consumidor recebe a mensagem,
// confirma o tratamento no banco e "morre" antes de apagá-la (o efeito é o de
// um kill -9 exatamente nesse ponto). A mensagem volta depois do visibility
// timeout e o consumidor real a reconhece pela inbox: nenhum novo débito, e a
// mensagem é apagada.
func TestConsumerCrashAfterCommitBeforeDelete(t *testing.T) {
	ctx := testContext(t)
	u := newUsecases(t, ctx, 3*time.Second)
	c := sqsClient(t, ctx)

	// Fila própria, com visibility timeout curto, para não disputar a
	// mensagem com os consumidores das réplicas.
	q, err := c.CreateQueue(ctx, &awssqs.CreateQueueInput{
		QueueName:  aws.String(fmt.Sprintf("crash-probe-%d.fifo", time.Now().UnixNano())),
		Attributes: map[string]string{"FifoQueue": "true", "VisibilityTimeout": "2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	queue := aws.ToString(q.QueueUrl)
	t.Cleanup(func() { _, _ = c.DeleteQueue(context.Background(), &awssqs.DeleteQueueInput{QueueUrl: q.QueueUrl}) })

	w := u.open(t, ctx, "100.00")
	in := op(t, w, "BET", "25.00", "")
	messageID := "msg-" + uuid.NewString()
	data := map[string]any{
		"providerId": in.ProviderID, "externalTransactionId": in.ExternalTransactionID,
		"idempotencyKey": keyFor(in).IdempotencyKey, "playerId": in.PlayerID, "walletId": in.WalletID,
		"roundId": in.RoundID, "gameId": in.GameID, "kind": in.Kind,
		"money": map[string]string{"amount": "25.00", "currency": "BRL"},
	}
	body := wagerMessage(messageID, data)
	send(t, ctx, c, queue, in.WalletID, messageID, body)

	// 1. O consumidor que vai cair: recebe e confirma o tratamento...
	out, err := c.ReceiveMessage(ctx, &awssqs.ReceiveMessageInput{
		QueueUrl: aws.String(queue), MaxNumberOfMessages: 1, WaitTimeSeconds: 2,
	})
	if err != nil || len(out.Messages) != 1 {
		t.Fatalf("receber: %v (%d mensagens)", err, len(out.Messages))
	}
	cmd, err := wagering.NewCommand(in)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(aws.ToString(out.Messages[0].Body)))
	first, err := u.wagers.ProcessFromMessage(ctx, cmd,
		wagering.Metadata{IdempotencyKey: keyFor(in).IdempotencyKey, CorrelationID: messageID},
		wagering.InboundMessage{ConsumerName: sqsadapter.ConsumerName, MessageID: messageID, PayloadHash: sum[:]})
	if err != nil || first.Transaction.Status() != wager.Processed {
		t.Fatalf("primeiro tratamento: %+v, %v", first, err)
	}
	// ...e morre aqui: não apaga a mensagem.

	if amount, _ := u.balance(t, ctx, w.ID()); amount != "75.00" {
		t.Fatalf("saldo após o commit = %s", amount)
	}

	// 2. O consumidor real (mesmo código das réplicas) recebe a reentrega.
	logs := &syncBuffer{}
	consumer := sqsadapter.NewConsumer(c, u.wagers, slog.New(slog.NewJSONHandler(logs, nil)),
		sqsadapter.ConsumerConfig{QueueURL: queue, DLQURL: queue, WaitTime: time.Second})
	runCtx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); consumer.Run(runCtx) }()
	defer func() { stop(); <-done }()

	eventually(t, 20*time.Second, "reentrega tratada e apagada", func() bool {
		attrs, err := c.GetQueueAttributes(ctx, &awssqs.GetQueueAttributesInput{
			QueueUrl: aws.String(queue),
			AttributeNames: []sqstypes.QueueAttributeName{
				sqstypes.QueueAttributeNameApproximateNumberOfMessages,
				sqstypes.QueueAttributeNameApproximateNumberOfMessagesNotVisible,
			},
		})
		return err == nil && strings.Contains(logs.String(), `"idempotentReplay":true`) &&
			attrs.Attributes["ApproximateNumberOfMessages"] == "0" &&
			attrs.Attributes["ApproximateNumberOfMessagesNotVisible"] == "0"
	})

	if amount, _ := u.balance(t, ctx, w.ID()); amount != "75.00" {
		t.Errorf("saldo após a reentrega = %s, want 75.00 (sem novo débito)", amount)
	}
	if n := u.count(t, ctx, `SELECT count(*) FROM wager_transactions WHERE external_transaction_id = $1`,
		in.ExternalTransactionID); n != 1 {
		t.Errorf("operações = %d, want 1", n)
	}
	if !strings.Contains(logs.String(), first.Transaction.ID().String()) {
		t.Errorf("a reentrega não devolveu a operação original: %s", logs.String())
	}
}
