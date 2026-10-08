//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gabrielmilano/wager-wallet-service/internal/adapter/postgres"
	"github.com/gabrielmilano/wager-wallet-service/internal/app/port"
	"github.com/gabrielmilano/wager-wallet-service/internal/app/wagering"
	"github.com/gabrielmilano/wager-wallet-service/internal/domain/wager"
)

// shiftedClock devolve o relógio real deslocado (para criar pendências já
// vencidas).
type shiftedClock struct{ offset time.Duration }

func (c shiftedClock) Now() time.Time { return time.Now().UTC().Add(c.offset) }

func txStatus(t *testing.T, ctx context.Context, u *usecases, id uuid.UUID) (wager.Status, wager.FailureCode) {
	t.Helper()
	tx, err := u.wagers.GetTransaction(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return tx.Status(), tx.FailureCode()
}

// A reversão chega antes da aposta; quando a aposta chega, o worker de uma
// das réplicas resolve a pendência (backoff a partir de 1 s).
func TestPendingReferenceResolvedByWorker(t *testing.T) {
	ctx := testContext(t)
	instances(t)
	u := newUsecases(t, ctx, 3*time.Second)
	w := u.open(t, ctx, "100.00")

	bet := op(t, w, "BET", "40.00", "")
	refund := u.mustProcess(t, ctx, op(t, w, "REFUND", "40.00", bet.ExternalTransactionID), wager.PendingReference)
	u.mustProcess(t, ctx, bet, wager.Processed)

	eventually(t, 20*time.Second, "REFUND resolvido pelo worker", func() bool {
		s, _ := txStatus(t, ctx, u, refund.ID())
		return s == wager.Processed
	})
	if amount, _ := u.balance(t, ctx, w.ID()); amount != "100.00" {
		t.Errorf("saldo = %s, want 100.00 (aposta devolvida)", amount)
	}
	u.reconcile(t, ctx, w.ID())
}

// Sem a referência, ao fim do prazo a pendência vira REJECTED
// REFERENCE_NOT_FOUND, com o evento de rejeição.
func TestPendingReferenceExpires(t *testing.T) {
	ctx := testContext(t)
	instances(t)
	u := newUsecases(t, ctx, 3*time.Second)
	w := u.open(t, ctx, "100.00")

	// Registrada "duas horas atrás": o prazo de 1 h já venceu.
	pool, err := postgres.NewPool(ctx, env("DATABASE_URL",
		"postgres://app_runtime:app_runtime@localhost:5432/wager_wallet?sslmode=disable"), 2)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	past := wagering.NewService(postgres.NewTxRunner(pool, 3*time.Second), shiftedClock{-2 * time.Hour}, port.UUIDv7{})
	in := op(t, w, "ROLLBACK", "10.00", "nunca-vai-chegar-"+uuid.NewString())
	cmd, err := wagering.NewCommand(in)
	if err != nil {
		t.Fatal(err)
	}
	res, err := past.Process(ctx, cmd, keyFor(in))
	if err != nil || res.Transaction.Status() != wager.PendingReference {
		t.Fatalf("pendência: %+v, %v", res, err)
	}

	eventually(t, 20*time.Second, "pendência expirada", func() bool {
		s, code := txStatus(t, ctx, u, res.Transaction.ID())
		return s == wager.Rejected && code == wager.ReferenceNotFound
	})
	types := u.eventTypes(t, ctx, w.ID())
	if types[len(types)-1] != "WagerTransactionRejected" {
		t.Errorf("último evento = %v", types)
	}
	if amount, _ := u.balance(t, ctx, w.ID()); amount != "100.00" {
		t.Errorf("saldo = %s", amount)
	}
}
