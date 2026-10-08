//go:build integration

package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// SQLSTATEs usados nas asserções.
const (
	sqlstateIntegrity = "23000" // integrity_constraint_violation (triggers de imutabilidade)
	sqlstateFK        = "23503" // foreign_key_violation
	sqlstateUnique    = "23505" // unique_violation
	sqlstateCheck     = "23514" // check_violation (CHECKs e triggers de regra)
	sqlstatePrivilege = "42501" // insufficient_privilege (GRANTs)
)

func runtimeConn(t *testing.T, ctx context.Context) *pgx.Conn {
	t.Helper()
	return connect(t, ctx, env("DATABASE_URL",
		"postgres://app_runtime:app_runtime@localhost:5432/wager_wallet?sslmode=disable"))
}

func migratorConn(t *testing.T, ctx context.Context) *pgx.Conn {
	t.Helper()
	return connect(t, ctx, env("MIGRATIONS_DATABASE_URL",
		"postgres://app_migrator:app_migrator@localhost:5432/wager_wallet?sslmode=disable"))
}

func newID(t *testing.T) uuid.UUID {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func ptr[T any](v T) *T { return &v }

// requirePgError confere SQLSTATE e, quando informado, o nome da constraint
// (que os triggers também preenchem; ADR 0011).
func requirePgError(t *testing.T, err error, code, constraint string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("esperado PgError %s/%s, veio %v", code, constraint, err)
	}
	if pgErr.Code != code || (constraint != "" && pgErr.ConstraintName != constraint) {
		t.Fatalf("erro = %s/%q (%s), want %s/%q", pgErr.Code, pgErr.ConstraintName, pgErr.Message, code, constraint)
	}
}

// inTx executa fn numa transação e faz commit. Devolve o erro de fn (com
// rollback) ou o do commit, onde aparecem os triggers diferidos.
func inTx(ctx context.Context, conn *pgx.Conn, fn func(tx pgx.Tx) error) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	return tx.Commit(ctx)
}

// rollbackOnly executa fn numa transação que sempre é desfeita. Usado em
// comandos destrutivos (TRUNCATE): se a proteção falhar, o teste falha sem
// apagar o banco compartilhado.
func rollbackOnly(ctx context.Context, conn *pgx.Conn, fn func(tx pgx.Tx) error) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	return fn(tx)
}

// execer é satisfeito por *pgx.Conn e por pgx.Tx.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// --- carteira ----------------------------------------------------------------

type wallet struct {
	ID, PlayerID uuid.UUID
	Currency     string
	Balance      int64
	Version      int64
}

func (w wallet) insert(ctx context.Context, db execer) error {
	now := time.Now()
	_, err := db.Exec(ctx,
		`INSERT INTO wallets (id, player_id, currency, balance_minor, version, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $6)`,
		w.ID, w.PlayerID, w.Currency, w.Balance, w.Version, now)
	return err
}

// setBalance muda saldo e versão como a aplicação fará: versão +1.
func (w *wallet) setBalance(ctx context.Context, db execer, balance int64) error {
	_, err := db.Exec(ctx,
		`UPDATE wallets SET balance_minor = $2, version = $3, updated_at = $4 WHERE id = $1`,
		w.ID, balance, w.Version+1, time.Now())
	if err == nil {
		w.Balance = balance
		w.Version++
	}
	return err
}

// openWallet abre e confirma uma carteira em BRL. Com saldo positivo, grava
// também a OPENING e o lançamento de crédito, na versão 1.
func openWallet(t *testing.T, ctx context.Context, conn *pgx.Conn, balance int64) *wallet {
	t.Helper()
	w := &wallet{ID: newID(t), PlayerID: newID(t), Currency: "BRL", Balance: balance, Version: 1}
	err := inTx(ctx, conn, func(tx pgx.Tx) error {
		if err := w.insert(ctx, tx); err != nil {
			return err
		}
		if balance == 0 {
			return nil
		}
		opening := openingTx(t, w, balance)
		if err := opening.insert(ctx, tx); err != nil {
			return err
		}
		return entryFor(t, w, opening, "CREDIT", 0).insert(ctx, tx)
	})
	if err != nil {
		t.Fatalf("abrir carteira: %v", err)
	}
	return w
}

// --- operação ----------------------------------------------------------------

type wagerTx struct {
	ID, WalletID, PlayerID          uuid.UUID
	Origin, Kind, Status, Currency  string
	Amount                          int64
	ProviderID, ExternalID, IdemKey *string
	RoundID, GameID, RefExternalID  *string
	PayloadHash                     []byte
	RefTxID                         *uuid.UUID
}

// externalTx monta uma operação externa válida em PENDING, com IDs únicos.
func externalTx(t *testing.T, w *wallet, kind string, amount int64) wagerTx {
	t.Helper()
	suffix := newID(t).String()
	return wagerTx{
		ID: newID(t), WalletID: w.ID, PlayerID: w.PlayerID,
		Origin: "EXTERNAL", Kind: kind, Status: "PENDING", Currency: w.Currency, Amount: amount,
		ProviderID: ptr("provider-test"), ExternalID: ptr("ext-" + suffix), IdemKey: ptr("key-" + suffix),
		RoundID: ptr("round-1"), GameID: ptr("game-1"), PayloadHash: []byte{0x01},
	}
}

// reversalTx monta um REFUND ou ROLLBACK que referencia original.
func reversalTx(t *testing.T, w *wallet, kind string, original wagerTx) wagerTx {
	t.Helper()
	x := externalTx(t, w, kind, original.Amount)
	x.RefExternalID = original.ExternalID
	x.RefTxID = &original.ID
	return x
}

func openingTx(t *testing.T, w *wallet, amount int64) wagerTx {
	t.Helper()
	return wagerTx{
		ID: newID(t), WalletID: w.ID, PlayerID: w.PlayerID,
		Origin: "INTERNAL", Kind: "OPENING", Status: "PROCESSED", Currency: w.Currency, Amount: amount,
	}
}

func (x wagerTx) insert(ctx context.Context, db execer) error {
	now := time.Now()
	var completedAt *time.Time
	if x.Status == "PROCESSED" || x.Status == "REJECTED" || x.Status == "FAILED" {
		completedAt = &now
	}
	_, err := db.Exec(ctx,
		`INSERT INTO wager_transactions (
		    id, origin, kind, status, wallet_id, player_id, currency, amount_minor,
		    provider_id, external_transaction_id, idempotency_key, payload_hash,
		    round_id, game_id, reference_external_id, reference_transaction_id,
		    created_at, updated_at, completed_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $17, $18)`,
		x.ID, x.Origin, x.Kind, x.Status, x.WalletID, x.PlayerID, x.Currency, x.Amount,
		x.ProviderID, x.ExternalID, x.IdemKey, x.PayloadHash,
		x.RoundID, x.GameID, x.RefExternalID, x.RefTxID,
		now, completedAt)
	return err
}

// finish leva a operação ao estado final, como a aplicação fará.
func finish(ctx context.Context, db execer, id uuid.UUID, status string, failureCode *string, resultBalance int64) error {
	_, err := db.Exec(ctx,
		`UPDATE wager_transactions
		    SET status = $2, failure_code = $3, result_balance_minor = $4,
		        updated_at = $5, completed_at = $5
		  WHERE id = $1`,
		id, status, failureCode, resultBalance, time.Now())
	return err
}

// --- extrato -----------------------------------------------------------------

type entry struct {
	ID, WalletID, TxID uuid.UUID
	Version            int64
	Direction          string
	Amount             int64
	Currency           string
	Before, After      int64
}

// entryFor monta o lançamento correto da operação x sobre o estado atual de w
// (antes = saldo atual; versão = atual + 1, ou 1 para a OPENING).
func entryFor(t *testing.T, w *wallet, x wagerTx, direction string, before int64) entry {
	t.Helper()
	after := before + x.Amount
	if direction == "DEBIT" {
		after = before - x.Amount
	}
	version := w.Version + 1
	if x.Kind == "OPENING" {
		version = 1
	}
	return entry{
		ID: newID(t), WalletID: w.ID, TxID: x.ID, Version: version,
		Direction: direction, Amount: x.Amount, Currency: w.Currency, Before: before, After: after,
	}
}

func (e entry) insert(ctx context.Context, db execer) error {
	_, err := db.Exec(ctx,
		`INSERT INTO wallet_ledger_entries (
		    id, wallet_id, transaction_id, wallet_version, direction, amount_minor,
		    currency, balance_before_minor, balance_after_minor, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		e.ID, e.WalletID, e.TxID, e.Version, e.Direction, e.Amount,
		e.Currency, e.Before, e.After, time.Now())
	return err
}

// process aplica uma operação financeira completa numa transação, na ordem
// que a aplicação usará: protocolo PENDING, saldo, extrato, estado final.
func process(t *testing.T, ctx context.Context, conn *pgx.Conn, w *wallet, x wagerTx, direction string) error {
	t.Helper()
	before := w.Balance
	e := entryFor(t, w, x, direction, before)
	snapshot := *w
	err := inTx(ctx, conn, func(tx pgx.Tx) error {
		if err := x.insert(ctx, tx); err != nil {
			return err
		}
		if err := w.setBalance(ctx, tx, e.After); err != nil {
			return err
		}
		if err := e.insert(ctx, tx); err != nil {
			return err
		}
		return finish(ctx, tx, x.ID, "PROCESSED", nil, e.After)
	})
	if err != nil {
		*w = snapshot // nada foi confirmado
	}
	return err
}
