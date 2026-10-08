package postgres

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gabrielmilano/wager-wallet-service/internal/app/store"
)

// NewPool cria o pool de conexões (sem conectar ainda: a primeira conexão
// acontece no Ping ou no primeiro uso).
func NewPool(ctx context.Context, databaseURL string, maxConns int32) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("DATABASE_URL inválida: %w", err)
	}
	if maxConns > 0 {
		cfg.MaxConns = maxConns
	}
	return pgxpool.NewWithConfig(ctx, cfg)
}

// TxRunner implementa store.TxRunner com uma transação pgx READ COMMITTED
// por chamada (ADR 0005).
type TxRunner struct {
	pool        *pgxpool.Pool
	lockTimeout time.Duration
}

func NewTxRunner(pool *pgxpool.Pool, lockTimeout time.Duration) *TxRunner {
	return &TxRunner{pool: pool, lockTimeout: lockTimeout}
}

// ReadSnapshot abre uma transação REPEATABLE READ READ ONLY: todas as
// leituras de fn veem o mesmo snapshot (usado na reconciliação).
func (r *TxRunner) ReadSnapshot(ctx context.Context, fn func(ctx context.Context, repos store.Repos) error) error {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return translate(err)
	}
	defer func() {
		rbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(rbCtx)
	}()
	repos := store.Repos{
		Wallets: &walletRepo{q: tx}, Transactions: &transactionRepo{q: tx}, Ledger: &ledgerRepo{q: tx},
		Inbox: &inboxRepo{q: tx}, Outbox: &outboxRepo{q: tx},
	}
	if err := fn(ctx, repos); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return translateCommit(err)
	}
	return nil
}

// WithinTx abre a transação, aplica o lock_timeout, entrega os repositórios
// ligados a ela e faz commit se fn devolver nil (rollback caso contrário).
func (r *TxRunner) WithinTx(ctx context.Context, fn func(ctx context.Context, repos store.Repos) error) error {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return translate(err)
	}
	// Sem efeito depois do commit; cobre retorno antecipado e panic. Usa um
	// contexto próprio para que o rollback aconteça mesmo com ctx cancelado.
	defer func() {
		rbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(rbCtx)
	}()

	// SET LOCAL: vale só para esta transação. Quem espera um lock além disso
	// recebe 55P03 (lock_not_available), tratado como transitório.
	if _, err := tx.Exec(ctx, `SELECT set_config('lock_timeout', $1, true)`,
		fmt.Sprintf("%dms", r.lockTimeout.Milliseconds())); err != nil {
		return translate(err)
	}

	repos := store.Repos{
		Wallets:      &walletRepo{q: tx},
		Transactions: &transactionRepo{q: tx},
		Ledger:       &ledgerRepo{q: tx},
		Inbox:        &inboxRepo{q: tx},
		Outbox:       &outboxRepo{q: tx},
	}
	if err := fn(ctx, repos); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return translateCommit(err)
	}
	return nil
}

// querier é o que os repositórios usam: pgx.Tx satisfaz.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// SQLSTATEs transitórios: repetir a operação pode dar certo.
var transientCodes = map[string]bool{
	"55P03": true,                               // lock_not_available (lock_timeout)
	"40001": true,                               // serialization_failure
	"40P01": true,                               // deadlock_detected
	"57P01": true,                               // admin_shutdown
	"57P02": true,                               // crash_shutdown
	"57P03": true,                               // cannot_connect_now
	"53300": true,                               // too_many_connections
	"08000": true, "08003": true, "08006": true, // connection_exception
}

// translate classifica um erro de comando SQL.
func translate(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return store.ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		if transientCodes[pgErr.Code] {
			return fmt.Errorf("%w: %s (%s)", store.ErrUnavailable, pgErr.Message, pgErr.Code)
		}
		return err // violação de regra ou bug: resultado conhecido, não transitório
	}
	if isConnectionError(err) {
		return fmt.Errorf("%w: %v", store.ErrUnavailable, err)
	}
	return err
}

// translateCommit aplica a regra do ADR 0005 (revisado): com resposta do
// servidor (PgError), o commit foi recusado e a transação desfeita — erro
// conhecido e definitivo, salvo os códigos transitórios; sem resposta, o
// resultado é desconhecido e tratado como transitório.
func translateCommit(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		if transientCodes[pgErr.Code] {
			return fmt.Errorf("%w: commit: %s (%s)", store.ErrUnavailable, pgErr.Message, pgErr.Code)
		}
		return fmt.Errorf("commit recusado: %w", err)
	}
	if errors.Is(err, context.Canceled) {
		return err
	}
	return fmt.Errorf("%w: resultado do commit desconhecido: %v", store.ErrUnavailable, err)
}

// isConnectionError: falhas sem resposta do servidor (rede, conexão recusada
// ou perdida, timeout).
func isConnectionError(err error) bool {
	var netErr net.Error
	var connectErr *pgconn.ConnectError
	return errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.As(err, &netErr) || errors.As(err, &connectErr) ||
		pgconn.Timeout(err) || pgconn.SafeToRetry(err)
}

// constraintViolation informa se err viola a constraint name.
func constraintViolation(err error, name string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.ConstraintName == name
}
