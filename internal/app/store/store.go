package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/gabrielmilano/wager-wallet-service/internal/domain/event"
	"github.com/gabrielmilano/wager-wallet-service/internal/domain/wager"
	"github.com/gabrielmilano/wager-wallet-service/internal/domain/wallet"
)

// Erros devolvidos pelas implementações, classificáveis com errors.Is.
var (
	ErrNotFound = errors.New("store: não encontrado")
	// ErrConflict: violação de unicidade esperada pelo chamador (ex.: carteira
	// duplicada para o mesmo jogador e moeda).
	ErrConflict = errors.New("store: conflito de unicidade")
	// ErrUnavailable: falha transitória (banco fora, lock_timeout, deadlock,
	// resultado de commit desconhecido). Repetir a operação é seguro.
	ErrUnavailable = errors.New("store: indisponível temporariamente")
)

// TxRunner delimita a transação SQL entre os repositórios (ADR 0005): os
// repositórios só existem dentro de fn. Erro em fn → rollback; nil → commit.
// Dentro de fn não há I/O externo.
type TxRunner interface {
	WithinTx(ctx context.Context, fn func(ctx context.Context, r Repos) error) error
}

// Repos são os repositórios ligados a uma mesma transação.
type Repos struct {
	Wallets      WalletRepository
	Transactions TransactionRepository
	Ledger       LedgerRepository
	Inbox        InboxRepository
	Outbox       OutboxRepository
}

type WalletRepository interface {
	// Insert grava uma carteira nova; ErrConflict se (jogador, moeda) já existe.
	Insert(ctx context.Context, w *wallet.Wallet) error
	// Get lê sem lock; ErrNotFound se não existe.
	Get(ctx context.Context, id uuid.UUID) (*wallet.Wallet, error)
	// GetForUpdate lê com SELECT ... FOR UPDATE: a fila por carteira.
	GetForUpdate(ctx context.Context, id uuid.UUID) (*wallet.Wallet, error)
	// UpdateBalance grava saldo e versão; confere que a versão gravada é a
	// anterior (proteção extra contra lost update, além do lock).
	UpdateBalance(ctx context.Context, w *wallet.Wallet) error
}

type TransactionRepository interface {
	// InsertPending grava uma operação externa em PENDING. Devolve false,
	// sem erro, se outra operação com o mesmo (provedor, id externo) ou
	// (provedor, chave) já existe: o chamador trata como replay ou conflito.
	InsertPending(ctx context.Context, tx *wager.Transaction) (bool, error)
	// InsertOpening grava a abertura interna (já PROCESSED).
	InsertOpening(ctx context.Context, tx *wager.Transaction) error
	// Update grava estado, referência resolvida, código, resultado e agenda
	// num único UPDATE (a máquina de estados do banco exige isso; ADR 0011).
	Update(ctx context.Context, tx *wager.Transaction) error
	GetByID(ctx context.Context, id uuid.UUID) (*wager.Transaction, error)
	FindByIdempotencyKey(ctx context.Context, providerID, key string) (*wager.Transaction, error)
	FindByExternalID(ctx context.Context, providerID, externalID string) (*wager.Transaction, error)
	// HasProcessedReversal informa se a operação já tem REFUND ou ROLLBACK
	// PROCESSED apontando para ela.
	HasProcessedReversal(ctx context.Context, originalID uuid.UUID) (bool, error)
	// LockDuePendingReference trava (FOR UPDATE SKIP LOCKED) até limit
	// operações em PENDING_REFERENCE com próxima tentativa vencida.
	LockDuePendingReference(ctx context.Context, now time.Time, limit int) ([]*wager.Transaction, error)
}

type LedgerRepository interface {
	Insert(ctx context.Context, e wallet.LedgerEntry) error
	// List devolve até limit lançamentos com versão > afterVersion, em ordem
	// crescente de versão (ordenação estável para a paginação).
	List(ctx context.Context, walletID uuid.UUID, afterVersion int64, limit int) ([]wallet.LedgerEntry, error)
}

// InboxMessage é o registro de uma mensagem recebida pelo consumidor.
type InboxMessage struct {
	ConsumerName  string
	MessageID     string
	PayloadHash   []byte
	TransactionID *uuid.UUID
	ReceivedAt    time.Time
	ProcessedAt   *time.Time
}

type InboxRepository interface {
	// Insert devolve false, sem erro, se a mensagem já foi registrada.
	Insert(ctx context.Context, m InboxMessage) (bool, error)
	Get(ctx context.Context, consumerName, messageID string) (InboxMessage, error)
	MarkProcessed(ctx context.Context, consumerName, messageID string, txID uuid.UUID, at time.Time) error
}

// OutboxEvent é um evento reservado para publicação.
type OutboxEvent struct {
	Envelope event.Envelope
	Payload  []byte // data já serializado (snapshot gravado no commit)
	Attempts int
}

type OutboxRepository interface {
	// Insert grava os eventos na mesma transação da operação.
	Insert(ctx context.Context, events ...event.Envelope) error
	// Claim reserva até limit eventos não publicados e vencidos, com lease
	// até leaseUntil para owner (FOR UPDATE SKIP LOCKED).
	Claim(ctx context.Context, owner string, now, leaseUntil time.Time, limit int) ([]OutboxEvent, error)
	MarkPublished(ctx context.Context, eventID uuid.UUID, owner string, at time.Time) error
	// MarkFailed libera o lease e agenda a próxima tentativa (backoff).
	MarkFailed(ctx context.Context, eventID uuid.UUID, owner string, nextAttempt time.Time, lastError string) error
}
