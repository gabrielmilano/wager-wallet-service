package event

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/gabrielmilano/wager-wallet-service/internal/domain/money"
	"github.com/gabrielmilano/wager-wallet-service/internal/domain/wager"
	"github.com/gabrielmilano/wager-wallet-service/internal/domain/wallet"
)

// ErrInvalidEvent indica dados insuficientes para montar um evento.
var ErrInvalidEvent = errors.New("event: evento inválido")

// Tipos de evento. A versão de cada um é definida pelo construtor.
const (
	TypeWagerTransactionProcessed        = "WagerTransactionProcessed"
	TypeWagerTransactionRejected         = "WagerTransactionRejected"
	TypeWalletBalanceChanged             = "WalletBalanceChanged"
	TypeWagerTransactionPendingReference = "WagerTransactionPendingReference"
)

// AggregateType de todos os eventos: a carteira (MessageGroupId = walletId).
const AggregateType = "Wallet"

// Envelope é o formato publicado. Data é um dos tipos concretos abaixo.
type Envelope struct {
	EventID       uuid.UUID `json:"eventId"`
	EventType     string    `json:"eventType"`
	AggregateID   uuid.UUID `json:"aggregateId"`
	CorrelationID string    `json:"correlationId"`
	CausationID   *string   `json:"causationId,omitempty"`
	OccurredAt    time.Time `json:"occurredAt"`
	Version       int       `json:"version"`
	Data          any       `json:"data"`
}

// Meta são os dados de rastreio comuns a todo evento.
type Meta struct {
	EventID       uuid.UUID
	CorrelationID string
	CausationID   string // opcional
	OccurredAt    time.Time
}

func newEnvelope(meta Meta, eventType string, version int, aggregateID uuid.UUID, data any) (Envelope, error) {
	if meta.EventID == uuid.Nil || meta.CorrelationID == "" || meta.OccurredAt.IsZero() || aggregateID == uuid.Nil {
		return Envelope{}, fmt.Errorf("%w: %s sem eventId, correlationId, occurredAt ou agregado", ErrInvalidEvent, eventType)
	}
	var causation *string
	if meta.CausationID != "" {
		c := meta.CausationID
		causation = &c
	}
	return Envelope{
		EventID:       meta.EventID,
		EventType:     eventType,
		AggregateID:   aggregateID,
		CorrelationID: meta.CorrelationID,
		CausationID:   causation,
		OccurredAt:    meta.OccurredAt.UTC(),
		Version:       version,
		Data:          data,
	}, nil
}

// TransactionData descreve a operação nos eventos de operação. Campos de
// provedor ficam ausentes na abertura interna (OPENING).
type TransactionData struct {
	TransactionID                  uuid.UUID   `json:"transactionId"`
	WalletID                       uuid.UUID   `json:"walletId"`
	PlayerID                       uuid.UUID   `json:"playerId"`
	Kind                           string      `json:"kind"`
	Status                         string      `json:"status"`
	Money                          money.Money `json:"money"`
	ProviderID                     string      `json:"providerId,omitempty"`
	ExternalTransactionID          string      `json:"externalTransactionId,omitempty"`
	RoundID                        string      `json:"roundId,omitempty"`
	GameID                         string      `json:"gameId,omitempty"`
	ReferenceExternalTransactionID string      `json:"referenceExternalTransactionId,omitempty"`
	ReferenceTransactionID         *uuid.UUID  `json:"referenceTransactionId,omitempty"`
}

func transactionData(tx *wager.Transaction) TransactionData {
	return TransactionData{
		TransactionID:                  tx.ID(),
		WalletID:                       tx.WalletID(),
		PlayerID:                       tx.PlayerID(),
		Kind:                           string(tx.Kind()),
		Status:                         string(tx.Status()),
		Money:                          tx.Money(),
		ProviderID:                     tx.ProviderID(),
		ExternalTransactionID:          tx.ExternalID(),
		RoundID:                        tx.RoundID(),
		GameID:                         tx.GameID(),
		ReferenceExternalTransactionID: tx.ReferenceExternalID(),
		ReferenceTransactionID:         tx.ReferenceTxID(),
	}
}

// WagerTransactionProcessed: conclusão bem-sucedida, inclusive LOSS e OPENING.
type WagerTransactionProcessed struct {
	TransactionData
	Balance money.Money `json:"balance"`
}

// WagerTransactionRejected: rejeição definitiva por regra de negócio.
type WagerTransactionRejected struct {
	TransactionData
	FailureCode string      `json:"failureCode"`
	Balance     money.Money `json:"balance"`
}

// WagerTransactionPendingReference: a operação espera a sua referência.
type WagerTransactionPendingReference struct {
	TransactionData
	ExpiresAt time.Time `json:"expiresAt"`
}

// WalletBalanceChanged: alteração efetiva do saldo (um por lançamento).
type WalletBalanceChanged struct {
	WalletID      uuid.UUID   `json:"walletId"`
	TransactionID uuid.UUID   `json:"transactionId"`
	Direction     string      `json:"direction"`
	Money         money.Money `json:"money"`
	BalanceBefore money.Money `json:"balanceBefore"`
	BalanceAfter  money.Money `json:"balanceAfter"`
	WalletVersion int64       `json:"walletVersion"`
}

// NewWagerTransactionProcessed monta o evento de uma operação PROCESSED.
func NewWagerTransactionProcessed(meta Meta, tx *wager.Transaction) (Envelope, error) {
	if tx.Status() != wager.Processed || tx.ResultBalance() == nil {
		return Envelope{}, fmt.Errorf("%w: operação %s não está PROCESSED", ErrInvalidEvent, tx.ID())
	}
	data := WagerTransactionProcessed{TransactionData: transactionData(tx), Balance: *tx.ResultBalance()}
	return newEnvelope(meta, TypeWagerTransactionProcessed, 1, tx.WalletID(), data)
}

// NewWagerTransactionRejected monta o evento de uma operação REJECTED.
func NewWagerTransactionRejected(meta Meta, tx *wager.Transaction) (Envelope, error) {
	if tx.Status() != wager.Rejected || tx.ResultBalance() == nil {
		return Envelope{}, fmt.Errorf("%w: operação %s não está REJECTED", ErrInvalidEvent, tx.ID())
	}
	data := WagerTransactionRejected{
		TransactionData: transactionData(tx),
		FailureCode:     string(tx.FailureCode()),
		Balance:         *tx.ResultBalance(),
	}
	return newEnvelope(meta, TypeWagerTransactionRejected, 1, tx.WalletID(), data)
}

// NewWagerTransactionPendingReference monta o evento de espera por referência.
func NewWagerTransactionPendingReference(meta Meta, tx *wager.Transaction) (Envelope, error) {
	if tx.Status() != wager.PendingReference || tx.ExpiresAt() == nil {
		return Envelope{}, fmt.Errorf("%w: operação %s não está PENDING_REFERENCE", ErrInvalidEvent, tx.ID())
	}
	data := WagerTransactionPendingReference{TransactionData: transactionData(tx), ExpiresAt: tx.ExpiresAt().UTC()}
	return newEnvelope(meta, TypeWagerTransactionPendingReference, 1, tx.WalletID(), data)
}

// NewWalletBalanceChanged monta o evento de um lançamento do extrato.
func NewWalletBalanceChanged(meta Meta, e wallet.LedgerEntry) (Envelope, error) {
	data := WalletBalanceChanged{
		WalletID:      e.WalletID(),
		TransactionID: e.TransactionID(),
		Direction:     string(e.Direction()),
		Money:         e.Amount(),
		BalanceBefore: e.BalanceBefore(),
		BalanceAfter:  e.BalanceAfter(),
		WalletVersion: e.WalletVersion(),
	}
	return newEnvelope(meta, TypeWalletBalanceChanged, 1, e.WalletID(), data)
}
