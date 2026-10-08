package sqs

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/gabrielmilano/wager-wallet-service/internal/app/store"
)

// NewClient cria o cliente SQS. Credenciais pela cadeia padrão da AWS
// (variáveis AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY no ambiente local);
// endpointURL vazio usa a AWS real.
func NewClient(ctx context.Context, region, endpointURL string) (*awssqs.Client, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("configuração AWS: %w", err)
	}
	return awssqs.NewFromConfig(cfg, func(o *awssqs.Options) {
		if endpointURL != "" {
			o.BaseEndpoint = aws.String(endpointURL)
		}
	}), nil
}

// QueueURL resolve a URL de uma fila pelo nome; falha se ela não existir.
func QueueURL(ctx context.Context, client *awssqs.Client, name string) (string, error) {
	out, err := client.GetQueueUrl(ctx, &awssqs.GetQueueUrlInput{QueueName: aws.String(name)})
	if err != nil {
		return "", fmt.Errorf("fila %s: %w", name, err)
	}
	return aws.ToString(out.QueueUrl), nil
}

// EventSender publica eventos da outbox em wallet-events.fifo.
//
// Contrato de roteamento: MessageGroupId = aggregateId (walletId), então os
// eventos de uma carteira saem em ordem; MessageDeduplicationId = eventId,
// então uma republicação (ex.: queda entre o envio e a confirmação na outbox)
// é descartada pelo SQS dentro da janela de 5 min e reconhecível pelo
// consumidor pelo eventId depois disso. Atributos eventType e aggregateType
// permitem filtrar sem abrir o corpo.
type EventSender struct {
	client   *awssqs.Client
	queueURL string
}

func NewEventSender(client *awssqs.Client, queueURL string) *EventSender {
	return &EventSender{client: client, queueURL: queueURL}
}

// eventBody é o envelope publicado; data é o snapshot gravado no commit.
type eventBody struct {
	EventID       string          `json:"eventId"`
	EventType     string          `json:"eventType"`
	AggregateID   string          `json:"aggregateId"`
	CorrelationID string          `json:"correlationId"`
	CausationID   *string         `json:"causationId,omitempty"`
	OccurredAt    string          `json:"occurredAt"`
	Version       int             `json:"version"`
	Data          json.RawMessage `json:"data"`
}

func (s *EventSender) Send(ctx context.Context, e store.OutboxEvent) error {
	env := e.Envelope
	body, err := json.Marshal(eventBody{
		EventID: env.EventID.String(), EventType: env.EventType, AggregateID: env.AggregateID.String(),
		CorrelationID: env.CorrelationID, CausationID: env.CausationID,
		OccurredAt: env.OccurredAt.UTC().Format("2006-01-02T15:04:05.000Z07:00"),
		Version:    env.Version, Data: e.Payload,
	})
	if err != nil {
		return err
	}
	_, err = s.client.SendMessage(ctx, &awssqs.SendMessageInput{
		QueueUrl:               aws.String(s.queueURL),
		MessageBody:            aws.String(string(body)),
		MessageGroupId:         aws.String(env.AggregateID.String()),
		MessageDeduplicationId: aws.String(env.EventID.String()),
		MessageAttributes: map[string]types.MessageAttributeValue{
			"eventType":     {DataType: aws.String("String"), StringValue: aws.String(env.EventType)},
			"aggregateType": {DataType: aws.String("String"), StringValue: aws.String("Wallet")},
		},
	})
	return err
}
