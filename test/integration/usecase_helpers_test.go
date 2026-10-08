//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gabrielmilano/wager-wallet-service/internal/adapter/postgres"
	"github.com/gabrielmilano/wager-wallet-service/internal/app/apperr"
	"github.com/gabrielmilano/wager-wallet-service/internal/app/port"
	"github.com/gabrielmilano/wager-wallet-service/internal/app/wagering"
	"github.com/gabrielmilano/wager-wallet-service/internal/app/wallets"
	"github.com/gabrielmilano/wager-wallet-service/internal/domain/money"
	"github.com/gabrielmilano/wager-wallet-service/internal/domain/wager"
	walletdom "github.com/gabrielmilano/wager-wallet-service/internal/domain/wallet"
)

// usecases reúne os casos de uso ligados ao PostgreSQL real, como app_runtime.
type usecases struct {
	pool    *pgxpool.Pool
	wagers  *wagering.Service
	wallets *wallets.Service
}

func newUsecases(t *testing.T, ctx context.Context, lockTimeout time.Duration) *usecases {
	t.Helper()
	pool, err := postgres.NewPool(ctx, env("DATABASE_URL",
		"postgres://app_runtime:app_runtime@localhost:5432/wager_wallet?sslmode=disable"), 20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	runner := postgres.NewTxRunner(pool, lockTimeout)
	return &usecases{
		pool:    pool,
		wagers:  wagering.NewService(runner, port.SystemClock{}, port.UUIDv7{}),
		wallets: wallets.NewService(runner, port.SystemClock{}, port.UUIDv7{}),
	}
}

func money2(t *testing.T, amount string) money.Money {
	t.Helper()
	m, err := money.Parse(amount, "BRL")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func (u *usecases) open(t *testing.T, ctx context.Context, amount string) *walletdom.Wallet {
	t.Helper()
	w, err := u.wallets.Open(ctx, wallets.OpenInput{PlayerID: uuid.NewString(), InitialBalance: money2(t, amount)})
	if err != nil {
		t.Fatalf("abrir carteira com %s: %v", amount, err)
	}
	return w
}

// op monta uma operação válida para a carteira, com id externo único.
func op(t *testing.T, w *walletdom.Wallet, kind, amount, ref string) wagering.Input {
	t.Helper()
	return wagering.Input{
		ProviderID: "provider-a", ExternalTransactionID: "ext-" + uuid.NewString(),
		PlayerID: w.PlayerID().String(), WalletID: w.ID().String(),
		RoundID: "round-1", GameID: "fortune-chimp", Kind: kind,
		Money: money2(t, amount), ReferenceExternalTransactionID: ref,
	}
}

func keyFor(in wagering.Input) wagering.Metadata {
	return wagering.Metadata{IdempotencyKey: in.ProviderID + ":" + in.ExternalTransactionID, CorrelationID: "corr-test"}
}

func (u *usecases) process(t *testing.T, ctx context.Context, in wagering.Input) (wagering.Result, error) {
	t.Helper()
	cmd, err := wagering.NewCommand(in)
	if err != nil {
		return wagering.Result{}, err
	}
	return u.wagers.Process(ctx, cmd, keyFor(in))
}

// mustProcess exige que a operação seja concluída (sem erro) com o estado
// esperado e devolve o resultado.
func (u *usecases) mustProcess(t *testing.T, ctx context.Context, in wagering.Input, status wager.Status) *wager.Transaction {
	t.Helper()
	res, err := u.process(t, ctx, in)
	if err != nil {
		t.Fatalf("%s %s: %v", in.Kind, in.Money.Amount(), err)
	}
	if got := res.Transaction.Status(); got != status {
		t.Fatalf("%s %s: estado %s (código %q), want %s", in.Kind, in.Money.Amount(), got, res.Transaction.FailureCode(), status)
	}
	return res.Transaction
}

func requireAppErr(t *testing.T, err error, kind apperr.Kind, code string) {
	t.Helper()
	e, ok := apperr.As(err)
	if !ok || e.Kind != kind || e.Code != code {
		t.Fatalf("erro = %v, want %s (classe %d)", err, code, kind)
	}
}

// balance lê saldo e versão gravados.
func (u *usecases) balance(t *testing.T, ctx context.Context, walletID uuid.UUID) (string, int64) {
	t.Helper()
	w, err := u.wallets.Get(ctx, walletID)
	if err != nil {
		t.Fatal(err)
	}
	return w.Balance().Amount(), w.Version()
}

// count conta linhas com uma consulta parametrizada.
func (u *usecases) count(t *testing.T, ctx context.Context, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := u.pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (u *usecases) ledgerCount(t *testing.T, ctx context.Context, walletID uuid.UUID) int {
	return u.count(t, ctx, `SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id = $1`, walletID)
}

// eventTypes devolve os tipos de evento da outbox de uma carteira, em ordem.
func (u *usecases) eventTypes(t *testing.T, ctx context.Context, walletID uuid.UUID) []string {
	t.Helper()
	rows, err := u.pool.Query(ctx,
		`SELECT event_type FROM outbox_events WHERE aggregate_id = $1 ORDER BY occurred_at, event_id`, walletID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

// reconcile confere que o saldo gravado é a soma do extrato.
func (u *usecases) reconcile(t *testing.T, ctx context.Context, walletID uuid.UUID) {
	t.Helper()
	var stored, calculated int64
	err := u.pool.QueryRow(ctx,
		`SELECT w.balance_minor,
		        COALESCE((SELECT SUM(CASE direction WHEN 'CREDIT' THEN amount_minor ELSE -amount_minor END)
		                    FROM wallet_ledger_entries WHERE wallet_id = w.id), 0)
		   FROM wallets w WHERE w.id = $1`, walletID).Scan(&stored, &calculated)
	if err != nil {
		t.Fatal(err)
	}
	if stored != calculated {
		t.Errorf("carteira %s: saldo %d, extrato soma %d", walletID, stored, calculated)
	}
}

func isUnavailable(err error) bool {
	e, ok := apperr.As(err)
	return ok && e.Kind == apperr.Unavailable
}
