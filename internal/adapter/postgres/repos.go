package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gabrielmilano/wager-wallet-service/internal/app/store"
	"github.com/gabrielmilano/wager-wallet-service/internal/domain/event"
	"github.com/gabrielmilano/wager-wallet-service/internal/domain/money"
	"github.com/gabrielmilano/wager-wallet-service/internal/domain/wager"
	"github.com/gabrielmilano/wager-wallet-service/internal/domain/wallet"
)

// --- wallets -----------------------------------------------------------------

type walletRepo struct{ q querier }

const walletColumns = `id, player_id, currency, balance_minor, version, created_at, updated_at`

func scanWallet(row pgx.Row) (*wallet.Wallet, error) {
	var (
		id, playerID         uuid.UUID
		currency             string
		balance, version     int64
		createdAt, updatedAt time.Time
	)
	if err := row.Scan(&id, &playerID, &currency, &balance, &version, &createdAt, &updatedAt); err != nil {
		return nil, translate(err)
	}
	m, err := money.New(balance, money.Currency(currency))
	if err != nil {
		return nil, err
	}
	return wallet.Rehydrate(id, playerID, m, version, createdAt, updatedAt)
}

func (r *walletRepo) Insert(ctx context.Context, w *wallet.Wallet) error {
	_, err := r.q.Exec(ctx,
		`INSERT INTO wallets (`+walletColumns+`) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		w.ID(), w.PlayerID(), string(w.Currency()), w.Balance().Minor(), w.Version(), w.CreatedAt(), w.UpdatedAt())
	if constraintViolation(err, "wallets_player_currency_key") {
		return store.ErrConflict
	}
	return translate(err)
}

func (r *walletRepo) Get(ctx context.Context, id uuid.UUID) (*wallet.Wallet, error) {
	return scanWallet(r.q.QueryRow(ctx, `SELECT `+walletColumns+` FROM wallets WHERE id = $1`, id))
}

// GetForUpdate é a fila por carteira: outra transação que peça o mesmo lock
// espera esta terminar (até o lock_timeout).
func (r *walletRepo) GetForUpdate(ctx context.Context, id uuid.UUID) (*wallet.Wallet, error) {
	return scanWallet(r.q.QueryRow(ctx, `SELECT `+walletColumns+` FROM wallets WHERE id = $1 FOR UPDATE`, id))
}

// UpdateBalance confere a versão anterior no WHERE: se alguém tivesse mudado
// a carteira sem o lock, nada é atualizado e o erro aparece (sem lost update).
func (r *walletRepo) UpdateBalance(ctx context.Context, w *wallet.Wallet) error {
	tag, err := r.q.Exec(ctx,
		`UPDATE wallets SET balance_minor = $2, version = $3, updated_at = $4
		  WHERE id = $1 AND version = $3 - 1`,
		w.ID(), w.Balance().Minor(), w.Version(), w.UpdatedAt())
	if err != nil {
		return translate(err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("carteira %s: versão %d não é a sucessora da gravada (atualização concorrente)", w.ID(), w.Version())
	}
	return nil
}

// --- wager_transactions ------------------------------------------------------

type transactionRepo struct{ q querier }

const transactionColumns = `id, origin, kind, status, wallet_id, player_id, currency, amount_minor,
	provider_id, external_transaction_id, idempotency_key, payload_hash, round_id, game_id,
	reference_external_id, reference_transaction_id, failure_code, result_balance_minor,
	attempts, next_attempt_at, expires_at, created_at, updated_at, completed_at`

func scanTransaction(row pgx.Row) (*wager.Transaction, error) {
	var (
		p                                     wager.RehydrateParams
		origin, kind, status, currency        string
		amount                                int64
		providerID, externalID, idemKey       *string
		roundID, gameID, refExternal, failure *string
		resultBalance                         *int64
	)
	err := row.Scan(&p.ID, &origin, &kind, &status, &p.WalletID, &p.PlayerID, &currency, &amount,
		&providerID, &externalID, &idemKey, &p.PayloadHash, &roundID, &gameID,
		&refExternal, &p.ReferenceTxID, &failure, &resultBalance,
		&p.Attempts, &p.NextAttemptAt, &p.ExpiresAt, &p.CreatedAt, &p.UpdatedAt, &p.CompletedAt)
	if err != nil {
		return nil, translate(err)
	}
	m, err := money.New(amount, money.Currency(currency))
	if err != nil {
		return nil, err
	}
	p.Origin, p.Kind, p.Status, p.Money = wager.Origin(origin), wager.Kind(kind), wager.Status(status), m
	p.ProviderID, p.ExternalID, p.IdempotencyKey = deref(providerID), deref(externalID), deref(idemKey)
	p.RoundID, p.GameID, p.ReferenceExternalID = deref(roundID), deref(gameID), deref(refExternal)
	p.FailureCode = wager.FailureCode(deref(failure))
	if resultBalance != nil {
		rb, err := money.New(*resultBalance, m.Currency())
		if err != nil {
			return nil, err
		}
		p.ResultBalance = &rb
	}
	return wager.Rehydrate(p)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// nullable devolve nil para string vazia (coluna NULL).
func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func minorOrNil(m *money.Money) *int64 {
	if m == nil {
		return nil
	}
	v := m.Minor()
	return &v
}

func (r *transactionRepo) insertSQL(ctx context.Context, tx *wager.Transaction, suffix string) (int64, error) {
	tag, err := r.q.Exec(ctx,
		`INSERT INTO wager_transactions (`+transactionColumns+`)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16,
		         $17, $18, $19, $20, $21, $22, $23, $24) `+suffix,
		tx.ID(), string(tx.Origin()), string(tx.Kind()), string(tx.Status()),
		tx.WalletID(), tx.PlayerID(), string(tx.Money().Currency()), tx.Money().Minor(),
		nullable(tx.ProviderID()), nullable(tx.ExternalID()), nullable(tx.IdempotencyKey()),
		tx.PayloadHash(), nullable(tx.RoundID()), nullable(tx.GameID()),
		nullable(tx.ReferenceExternalID()), tx.ReferenceTxID(), nullable(string(tx.FailureCode())),
		minorOrNil(tx.ResultBalance()), tx.Attempts(), tx.NextAttemptAt(), tx.ExpiresAt(),
		tx.CreatedAt(), tx.UpdatedAt(), tx.CompletedAt())
	if err != nil {
		return 0, translate(err)
	}
	return tag.RowsAffected(), nil
}

// InsertPending usa ON CONFLICT DO NOTHING: se outra transação já gravou a
// mesma operação (mesmo id externo ou chave do provedor), este INSERT espera
// ela terminar e não grava nada; o chamador então lê o resultado dela.
func (r *transactionRepo) InsertPending(ctx context.Context, tx *wager.Transaction) (bool, error) {
	n, err := r.insertSQL(ctx, tx, `ON CONFLICT DO NOTHING`)
	return n == 1, err
}

func (r *transactionRepo) InsertOpening(ctx context.Context, tx *wager.Transaction) error {
	_, err := r.insertSQL(ctx, tx, "")
	return err
}

func (r *transactionRepo) Update(ctx context.Context, tx *wager.Transaction) error {
	tag, err := r.q.Exec(ctx,
		`UPDATE wager_transactions
		    SET status = $2, reference_transaction_id = $3, failure_code = $4,
		        result_balance_minor = $5, attempts = $6, next_attempt_at = $7,
		        expires_at = $8, updated_at = $9, completed_at = $10
		  WHERE id = $1`,
		tx.ID(), string(tx.Status()), tx.ReferenceTxID(), nullable(string(tx.FailureCode())),
		minorOrNil(tx.ResultBalance()), tx.Attempts(), tx.NextAttemptAt(),
		tx.ExpiresAt(), tx.UpdatedAt(), tx.CompletedAt())
	if err != nil {
		return translate(err)
	}
	if tag.RowsAffected() != 1 {
		return store.ErrNotFound
	}
	return nil
}

func (r *transactionRepo) GetByID(ctx context.Context, id uuid.UUID) (*wager.Transaction, error) {
	return scanTransaction(r.q.QueryRow(ctx,
		`SELECT `+transactionColumns+` FROM wager_transactions WHERE id = $1`, id))
}

// As buscas repetem o predicado origin = 'EXTERNAL' dos índices únicos
// parciais, para que o planner possa usá-los.
func (r *transactionRepo) FindByIdempotencyKey(ctx context.Context, providerID, key string) (*wager.Transaction, error) {
	return scanTransaction(r.q.QueryRow(ctx,
		`SELECT `+transactionColumns+` FROM wager_transactions
		  WHERE origin = 'EXTERNAL' AND provider_id = $1 AND idempotency_key = $2`, providerID, key))
}

func (r *transactionRepo) FindByExternalID(ctx context.Context, providerID, externalID string) (*wager.Transaction, error) {
	return scanTransaction(r.q.QueryRow(ctx,
		`SELECT `+transactionColumns+` FROM wager_transactions
		  WHERE origin = 'EXTERNAL' AND provider_id = $1 AND external_transaction_id = $2`, providerID, externalID))
}

func (r *transactionRepo) HasProcessedReversal(ctx context.Context, originalID uuid.UUID) (bool, error) {
	var exists bool
	err := r.q.QueryRow(ctx,
		`SELECT EXISTS (
		    SELECT 1 FROM wager_transactions
		     WHERE reference_transaction_id = $1 AND status = 'PROCESSED'
		       AND kind IN ('REFUND', 'ROLLBACK'))`, originalID).Scan(&exists)
	return exists, translate(err)
}

// LockDuePendingReference: SKIP LOCKED faz várias instâncias dividirem as
// pendências sem esperar umas pelas outras.
func (r *transactionRepo) LockDuePendingReference(ctx context.Context, now time.Time, limit int) ([]*wager.Transaction, error) {
	rows, err := r.q.Query(ctx,
		`SELECT `+transactionColumns+` FROM wager_transactions
		  WHERE status = 'PENDING_REFERENCE' AND next_attempt_at <= $1
		  ORDER BY next_attempt_at
		  LIMIT $2
		  FOR UPDATE SKIP LOCKED`, now, limit)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()
	var out []*wager.Transaction
	for rows.Next() {
		tx, err := scanTransaction(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, tx)
	}
	return out, translate(rows.Err())
}

// --- wallet_ledger_entries ---------------------------------------------------

type ledgerRepo struct{ q querier }

const ledgerColumns = `id, wallet_id, transaction_id, wallet_version, direction, amount_minor,
	currency, balance_before_minor, balance_after_minor, created_at`

func (r *ledgerRepo) Insert(ctx context.Context, e wallet.LedgerEntry) error {
	_, err := r.q.Exec(ctx,
		`INSERT INTO wallet_ledger_entries (`+ledgerColumns+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		e.ID(), e.WalletID(), e.TransactionID(), e.WalletVersion(), string(e.Direction()),
		e.Amount().Minor(), string(e.Amount().Currency()),
		e.BalanceBefore().Minor(), e.BalanceAfter().Minor(), e.CreatedAt())
	return translate(err)
}

func (r *ledgerRepo) List(ctx context.Context, walletID uuid.UUID, afterVersion int64, limit int) ([]wallet.LedgerEntry, error) {
	rows, err := r.q.Query(ctx,
		`SELECT `+ledgerColumns+` FROM wallet_ledger_entries
		  WHERE wallet_id = $1 AND wallet_version > $2
		  ORDER BY wallet_version
		  LIMIT $3`, walletID, afterVersion, limit)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	var out []wallet.LedgerEntry
	for rows.Next() {
		var (
			p                     wallet.EntryParams
			direction, currency   string
			amount, before, after int64
		)
		if err := rows.Scan(&p.ID, &p.WalletID, &p.TransactionID, &p.WalletVersion, &direction,
			&amount, &currency, &before, &after, &p.CreatedAt); err != nil {
			return nil, translate(err)
		}
		c := money.Currency(currency)
		var errs [3]error
		p.Amount, errs[0] = money.New(amount, c)
		p.BalanceBefore, errs[1] = money.New(before, c)
		p.BalanceAfter, errs[2] = money.New(after, c)
		for _, err := range errs {
			if err != nil {
				return nil, err
			}
		}
		p.Direction = wallet.Direction(direction)
		e, err := wallet.NewLedgerEntry(p)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, translate(rows.Err())
}

// --- inbox_messages ----------------------------------------------------------

type inboxRepo struct{ q querier }

// Insert com ON CONFLICT DO NOTHING: uma reentrega concorrente espera a
// primeira terminar e depois encontra o registro dela.
func (r *inboxRepo) Insert(ctx context.Context, m store.InboxMessage) (bool, error) {
	tag, err := r.q.Exec(ctx,
		`INSERT INTO inbox_messages (consumer_name, message_id, payload_hash, received_at)
		 VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING`,
		m.ConsumerName, m.MessageID, m.PayloadHash, m.ReceivedAt)
	if err != nil {
		return false, translate(err)
	}
	return tag.RowsAffected() == 1, nil
}

func (r *inboxRepo) Get(ctx context.Context, consumerName, messageID string) (store.InboxMessage, error) {
	m := store.InboxMessage{ConsumerName: consumerName, MessageID: messageID}
	err := r.q.QueryRow(ctx,
		`SELECT payload_hash, transaction_id, received_at, processed_at
		   FROM inbox_messages WHERE consumer_name = $1 AND message_id = $2`,
		consumerName, messageID).Scan(&m.PayloadHash, &m.TransactionID, &m.ReceivedAt, &m.ProcessedAt)
	return m, translate(err)
}

func (r *inboxRepo) MarkProcessed(ctx context.Context, consumerName, messageID string, txID uuid.UUID, at time.Time) error {
	_, err := r.q.Exec(ctx,
		`UPDATE inbox_messages SET transaction_id = $3, processed_at = $4
		  WHERE consumer_name = $1 AND message_id = $2`, consumerName, messageID, txID, at)
	return translate(err)
}

// --- outbox_events -----------------------------------------------------------

type outboxRepo struct{ q querier }

func (r *outboxRepo) Insert(ctx context.Context, events ...event.Envelope) error {
	for _, e := range events {
		payload, err := json.Marshal(e.Data)
		if err != nil {
			return fmt.Errorf("serializar evento %s: %w", e.EventType, err)
		}
		_, err = r.q.Exec(ctx,
			`INSERT INTO outbox_events (event_id, aggregate_type, aggregate_id, event_type, event_version,
			                            correlation_id, causation_id, payload, occurred_at, next_attempt_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $9)`,
			e.EventID, event.AggregateType, e.AggregateID, e.EventType, e.Version,
			e.CorrelationID, e.CausationID, payload, e.OccurredAt)
		if err != nil {
			return translate(err)
		}
	}
	return nil
}

// Claim reserva eventos vencidos e sem lease válido. SKIP LOCKED: publishers
// concorrentes pegam lotes diferentes; o lease (locked_until) devolve à fila
// o lote de um publisher que caiu.
func (r *outboxRepo) Claim(ctx context.Context, owner string, now, leaseUntil time.Time, limit int) ([]store.OutboxEvent, error) {
	rows, err := r.q.Query(ctx,
		`UPDATE outbox_events
		    SET locked_by = $1, locked_until = $3, attempts = attempts + 1
		  WHERE event_id IN (
		        SELECT event_id FROM outbox_events
		         WHERE published_at IS NULL
		           AND next_attempt_at <= $2
		           AND (locked_until IS NULL OR locked_until < $2)
		         ORDER BY occurred_at, event_id
		         LIMIT $4
		         FOR UPDATE SKIP LOCKED)
		 RETURNING event_id, aggregate_id, event_type, event_version, correlation_id,
		           causation_id, payload, occurred_at, attempts`,
		owner, now, leaseUntil, limit)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	var out []store.OutboxEvent
	for rows.Next() {
		var e store.OutboxEvent
		if err := rows.Scan(&e.Envelope.EventID, &e.Envelope.AggregateID, &e.Envelope.EventType,
			&e.Envelope.Version, &e.Envelope.CorrelationID, &e.Envelope.CausationID, &e.Payload,
			&e.Envelope.OccurredAt, &e.Attempts); err != nil {
			return nil, translate(err)
		}
		e.Envelope.OccurredAt = e.Envelope.OccurredAt.UTC()
		e.Envelope.Data = json.RawMessage(e.Payload)
		out = append(out, e)
	}
	return out, translate(rows.Err())
}

func (r *outboxRepo) MarkPublished(ctx context.Context, eventID uuid.UUID, owner string, at time.Time) error {
	tag, err := r.q.Exec(ctx,
		`UPDATE outbox_events SET published_at = $3, locked_by = NULL, locked_until = NULL
		  WHERE event_id = $1 AND locked_by = $2 AND published_at IS NULL`, eventID, owner, at)
	if err != nil {
		return translate(err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%w: evento %s não está reservado por %s", store.ErrNotFound, eventID, owner)
	}
	return nil
}

func (r *outboxRepo) MarkFailed(ctx context.Context, eventID uuid.UUID, owner string, nextAttempt time.Time, lastError string) error {
	tag, err := r.q.Exec(ctx,
		`UPDATE outbox_events
		    SET locked_by = NULL, locked_until = NULL, next_attempt_at = $3, last_error = $4
		  WHERE event_id = $1 AND locked_by = $2 AND published_at IS NULL`,
		eventID, owner, nextAttempt, lastError)
	if err != nil {
		return translate(err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%w: evento %s não está reservado por %s", store.ErrNotFound, eventID, owner)
	}
	return nil
}
