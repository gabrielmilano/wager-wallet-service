//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// Os testes deste arquivo provam que o próprio banco recusa estados
// inválidos (ADR 0011). Rodam como app_runtime, o role da aplicação; quando
// a permissão já barra o app_runtime, repetem como app_migrator para provar
// que o trigger também barra. Cada teste cria os próprios dados (ADR 0007).

// --- wallets -----------------------------------------------------------------

func TestWalletRejectsInvalidRows(t *testing.T) {
	ctx := testContext(t)
	conn := runtimeConn(t, ctx)

	tests := []struct {
		name       string
		change     func(w *wallet)
		code       string
		constraint string
	}{
		{"saldo negativo", func(w *wallet) { w.Balance = -1 }, sqlstateCheck, "wallets_balance_non_negative"},
		{"moeda fora da lista", func(w *wallet) { w.Currency = "GBP" }, sqlstateCheck, "wallets_currency_supported"},
		{"moeda minúscula", func(w *wallet) { w.Currency = "brl" }, sqlstateCheck, "wallets_currency_supported"},
		{"nasce na versão 2", func(w *wallet) { w.Version = 2 }, sqlstateCheck, "wallets_version_rule"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := wallet{ID: newID(t), PlayerID: newID(t), Currency: "BRL", Version: 1}
			tt.change(&w)
			requirePgError(t, w.insert(ctx, conn), tt.code, tt.constraint)
		})
	}
}

func TestWalletUniquePerPlayerAndCurrency(t *testing.T) {
	ctx := testContext(t)
	conn := runtimeConn(t, ctx)
	w := openWallet(t, ctx, conn, 0)

	dup := wallet{ID: newID(t), PlayerID: w.PlayerID, Currency: "BRL", Version: 1}
	requirePgError(t, dup.insert(ctx, conn), sqlstateUnique, "wallets_player_currency_key")

	// Outra moeda para o mesmo jogador é permitida.
	usd := wallet{ID: newID(t), PlayerID: w.PlayerID, Currency: "USD", Version: 1}
	if err := usd.insert(ctx, conn); err != nil {
		t.Errorf("carteira USD do mesmo jogador: %v", err)
	}
}

func TestWalletVersionFollowsBalance(t *testing.T) {
	ctx := testContext(t)
	conn := runtimeConn(t, ctx)
	w := openWallet(t, ctx, conn, 10000)

	_, err := conn.Exec(ctx, `UPDATE wallets SET version = version + 1 WHERE id = $1`, w.ID)
	requirePgError(t, err, sqlstateCheck, "wallets_version_rule")

	_, err = conn.Exec(ctx, `UPDATE wallets SET balance_minor = 500 WHERE id = $1`, w.ID)
	requirePgError(t, err, sqlstateCheck, "wallets_version_rule")

	_, err = conn.Exec(ctx, `UPDATE wallets SET balance_minor = 500, version = version + 2 WHERE id = $1`, w.ID)
	requirePgError(t, err, sqlstateCheck, "wallets_version_rule")
}

func TestWalletIdentityIsImmutable(t *testing.T) {
	ctx := testContext(t)
	w := openWallet(t, ctx, runtimeConn(t, ctx), 0)

	// app_runtime nem tem permissão de UPDATE nessas colunas.
	_, err := runtimeConn(t, ctx).Exec(ctx, `UPDATE wallets SET player_id = $2 WHERE id = $1`, w.ID, newID(t))
	requirePgError(t, err, sqlstatePrivilege, "")

	// O dono da tabela tem a permissão, e o trigger barra.
	mig := migratorConn(t, ctx)
	_, err = mig.Exec(ctx, `UPDATE wallets SET player_id = $2 WHERE id = $1`, w.ID, newID(t))
	requirePgError(t, err, sqlstateIntegrity, "wallets_identity_immutable")
	_, err = mig.Exec(ctx, `UPDATE wallets SET currency = 'USD' WHERE id = $1`, w.ID)
	requirePgError(t, err, sqlstateIntegrity, "wallets_identity_immutable")
}

func TestWalletCannotBeDeletedByRuntime(t *testing.T) {
	ctx := testContext(t)
	conn := runtimeConn(t, ctx)
	w := openWallet(t, ctx, conn, 0)

	_, err := conn.Exec(ctx, `DELETE FROM wallets WHERE id = $1`, w.ID)
	requirePgError(t, err, sqlstatePrivilege, "")
}

// E1: saldo e versão da carteira precisam bater com o último lançamento no
// commit, por qualquer lado que a divergência seja criada.
func TestWalletMustMatchLedgerAtCommit(t *testing.T) {
	ctx := testContext(t)
	conn := runtimeConn(t, ctx)

	t.Run("carteira nasce com saldo sem lançamento", func(t *testing.T) {
		w := wallet{ID: newID(t), PlayerID: newID(t), Currency: "BRL", Balance: 500, Version: 1}
		err := inTx(ctx, conn, func(tx pgx.Tx) error { return w.insert(ctx, tx) })
		requirePgError(t, err, sqlstateCheck, "wallet_ledger_consistency")
	})

	t.Run("saldo muda sem lançamento", func(t *testing.T) {
		w := openWallet(t, ctx, conn, 10000)
		err := inTx(ctx, conn, func(tx pgx.Tx) error { return w.setBalance(ctx, tx, 9000) })
		requirePgError(t, err, sqlstateCheck, "wallet_ledger_consistency")
	})

	t.Run("lançamento sem mudar o saldo", func(t *testing.T) {
		w := openWallet(t, ctx, conn, 10000)
		bet := externalTx(t, w, "BET", 8000)
		e := entryFor(t, w, bet, "DEBIT", w.Balance)
		err := inTx(ctx, conn, func(tx pgx.Tx) error {
			if err := bet.insert(ctx, tx); err != nil {
				return err
			}
			if err := e.insert(ctx, tx); err != nil {
				return err
			}
			return finish(ctx, tx, bet.ID, "PROCESSED", nil, e.After)
		})
		requirePgError(t, err, sqlstateCheck, "wallet_ledger_consistency")
	})
}

// --- wager_transactions ------------------------------------------------------

func TestWagerTransactionRejectsInvalidShape(t *testing.T) {
	ctx := testContext(t)
	conn := runtimeConn(t, ctx)
	w := openWallet(t, ctx, conn, 0)

	tests := []struct {
		name       string
		build      func() wagerTx
		code       string
		constraint string
	}{
		{"OPENING com dado de provedor", func() wagerTx {
			x := openingTx(t, w, 100)
			x.ProviderID = ptr("provider-test")
			return x
		}, sqlstateCheck, "wager_tx_origin_shape"},
		{"externa sem rodada", func() wagerTx {
			x := externalTx(t, w, "BET", 100)
			x.RoundID = nil
			return x
		}, sqlstateCheck, "wager_tx_origin_shape"},
		{"externa do tipo OPENING", func() wagerTx {
			return externalTx(t, w, "OPENING", 100)
		}, sqlstateCheck, "wager_tx_origin_shape"},
		{"LOSS com valor", func() wagerTx { return externalTx(t, w, "LOSS", 100) }, sqlstateCheck, "wager_tx_amount_by_kind"},
		{"BET com zero", func() wagerTx { return externalTx(t, w, "BET", 0) }, sqlstateCheck, "wager_tx_amount_by_kind"},
		{"REFUND sem referência", func() wagerTx { return externalTx(t, w, "REFUND", 100) }, sqlstateCheck, "wager_tx_reference_required"},
		{"referência a si mesma", func() wagerTx {
			x := externalTx(t, w, "ROLLBACK", 100)
			x.RefExternalID = ptr("ext-qualquer")
			x.RefTxID = &x.ID
			return x
		}, sqlstateCheck, "wager_tx_no_self_reference"},
		{"moeda diferente da carteira", func() wagerTx {
			x := externalTx(t, w, "BET", 100)
			x.Currency = "USD"
			return x
		}, sqlstateFK, "wager_tx_wallet_fkey"},
		{"jogador diferente da carteira", func() wagerTx {
			x := externalTx(t, w, "BET", 100)
			x.PlayerID = newID(t)
			return x
		}, sqlstateFK, "wager_tx_wallet_fkey"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requirePgError(t, tt.build().insert(ctx, conn), tt.code, tt.constraint)
		})
	}
}

func TestWagerTransactionUniqueness(t *testing.T) {
	ctx := testContext(t)
	conn := runtimeConn(t, ctx)
	w := openWallet(t, ctx, conn, 10000)

	t.Run("mesmo ID externo com outra chave", func(t *testing.T) {
		first := externalTx(t, w, "LOSS", 0)
		second := externalTx(t, w, "LOSS", 0)
		second.ExternalID = first.ExternalID
		err := inTx(ctx, conn, func(tx pgx.Tx) error {
			if err := first.insert(ctx, tx); err != nil {
				return err
			}
			return second.insert(ctx, tx)
		})
		requirePgError(t, err, sqlstateUnique, "ux_tx_provider_external")
	})

	t.Run("mesma chave de idempotência", func(t *testing.T) {
		first := externalTx(t, w, "LOSS", 0)
		second := externalTx(t, w, "LOSS", 0)
		second.IdemKey = first.IdemKey
		err := inTx(ctx, conn, func(tx pgx.Tx) error {
			if err := first.insert(ctx, tx); err != nil {
				return err
			}
			return second.insert(ctx, tx)
		})
		requirePgError(t, err, sqlstateUnique, "ux_tx_provider_idem_key")
	})

	t.Run("segunda OPENING", func(t *testing.T) {
		requirePgError(t, openingTx(t, w, 100).insert(ctx, conn), sqlstateUnique, "ux_tx_single_opening")
	})

	t.Run("segunda reversão processada da mesma aposta", func(t *testing.T) {
		bet := externalTx(t, w, "BET", 1000)
		if err := process(t, ctx, conn, w, bet, "DEBIT"); err != nil {
			t.Fatal(err)
		}
		if err := process(t, ctx, conn, w, reversalTx(t, w, "REFUND", bet), "CREDIT"); err != nil {
			t.Fatalf("primeiro REFUND: %v", err)
		}
		err := process(t, ctx, conn, w, reversalTx(t, w, "ROLLBACK", bet), "CREDIT")
		requirePgError(t, err, sqlstateUnique, "ux_tx_single_reversal")
	})
}

// E4: transições e congelamento.
func TestWagerTransactionStateMachine(t *testing.T) {
	ctx := testContext(t)
	conn := runtimeConn(t, ctx)
	w := openWallet(t, ctx, conn, 10000)

	t.Run("externa não nasce PROCESSED", func(t *testing.T) {
		x := externalTx(t, w, "LOSS", 0)
		x.Status = "PROCESSED"
		requirePgError(t, x.insert(ctx, conn), sqlstateCheck, "wager_tx_transition")
	})

	t.Run("OPENING não nasce PENDING", func(t *testing.T) {
		other := openWallet(t, ctx, conn, 0)
		x := openingTx(t, other, 100)
		x.Status = "PENDING"
		requirePgError(t, x.insert(ctx, conn), sqlstateCheck, "wager_tx_transition")
	})

	t.Run("PENDING não vai para FAILED", func(t *testing.T) {
		x := externalTx(t, w, "LOSS", 0)
		err := inTx(ctx, conn, func(tx pgx.Tx) error {
			if err := x.insert(ctx, tx); err != nil {
				return err
			}
			return finish(ctx, tx, x.ID, "FAILED", ptr("X"), 0)
		})
		requirePgError(t, err, sqlstateCheck, "wager_tx_transition")
	})

	t.Run("PENDING não fica PENDING", func(t *testing.T) {
		x := externalTx(t, w, "LOSS", 0)
		err := inTx(ctx, conn, func(tx pgx.Tx) error {
			if err := x.insert(ctx, tx); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `UPDATE wager_transactions SET attempts = 1 WHERE id = $1`, x.ID)
			return err
		})
		requirePgError(t, err, sqlstateCheck, "wager_tx_transition")
	})

	t.Run("PENDING_REFERENCE até FAILED é permitido", func(t *testing.T) {
		x := reversalTx(t, w, "REFUND", externalTx(t, w, "BET", 100))
		x.RefTxID = nil // a original ainda não chegou
		err := inTx(ctx, conn, func(tx pgx.Tx) error {
			if err := x.insert(ctx, tx); err != nil {
				return err
			}
			now := time.Now()
			if _, err := tx.Exec(ctx,
				`UPDATE wager_transactions
				    SET status = 'PENDING_REFERENCE', next_attempt_at = $2, expires_at = $3, updated_at = $2
				  WHERE id = $1`, x.ID, now, now.Add(time.Hour)); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx,
				`UPDATE wager_transactions SET attempts = attempts + 1 WHERE id = $1`, x.ID); err != nil {
				return err
			}
			return finish(ctx, tx, x.ID, "FAILED", ptr("PERMANENT_ERROR"), 0)
		})
		if err != nil {
			t.Errorf("PENDING -> PENDING_REFERENCE -> PENDING_REFERENCE -> FAILED: %v", err)
		}
	})

	t.Run("estado terminal congelado", func(t *testing.T) {
		bet := externalTx(t, w, "BET", 100)
		if err := process(t, ctx, conn, w, bet, "DEBIT"); err != nil {
			t.Fatal(err)
		}
		_, err := conn.Exec(ctx, `UPDATE wager_transactions SET attempts = 1 WHERE id = $1`, bet.ID)
		requirePgError(t, err, sqlstateIntegrity, "wager_tx_terminal")
	})
}

func TestWagerTransactionCoherenceChecks(t *testing.T) {
	ctx := testContext(t)
	conn := runtimeConn(t, ctx)
	w := openWallet(t, ctx, conn, 0)

	tests := []struct {
		name       string
		update     string
		constraint string
	}{
		{"REJECTED sem failure_code",
			`UPDATE wager_transactions SET status = 'REJECTED', completed_at = now() WHERE id = $1`,
			"wager_tx_failure_code_coherent"},
		{"PROCESSED sem completed_at",
			`UPDATE wager_transactions SET status = 'PROCESSED' WHERE id = $1`,
			"wager_tx_completed_at_coherent"},
		{"PENDING_REFERENCE sem agenda",
			`UPDATE wager_transactions SET status = 'PENDING_REFERENCE' WHERE id = $1`,
			"wager_tx_pending_reference_schedule"},
		{"tentativas negativas",
			`UPDATE wager_transactions SET status = 'PROCESSED', completed_at = now(), attempts = -1 WHERE id = $1`,
			"wager_tx_attempts_non_negative"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			x := externalTx(t, w, "LOSS", 0)
			err := inTx(ctx, conn, func(tx pgx.Tx) error {
				if err := x.insert(ctx, tx); err != nil {
					return err
				}
				_, err := tx.Exec(ctx, tt.update, x.ID)
				return err
			})
			requirePgError(t, err, sqlstateCheck, tt.constraint)
		})
	}
}

func TestWagerTransactionBusinessFieldsAreImmutable(t *testing.T) {
	ctx := testContext(t)
	conn := runtimeConn(t, ctx)
	w := openWallet(t, ctx, conn, 0)

	// Fica em PENDING_REFERENCE (não terminal) para isolar a regra de imutabilidade.
	x := externalTx(t, w, "REFUND", 100)
	x.RefExternalID = ptr("ext-que-nao-chegou")
	err := inTx(ctx, conn, func(tx pgx.Tx) error {
		if err := x.insert(ctx, tx); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`UPDATE wager_transactions
			    SET status = 'PENDING_REFERENCE', next_attempt_at = now(), expires_at = now() + interval '1 hour'
			  WHERE id = $1`, x.ID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = conn.Exec(ctx, `UPDATE wager_transactions SET amount_minor = 1 WHERE id = $1`, x.ID)
	requirePgError(t, err, sqlstatePrivilege, "")

	mig := migratorConn(t, ctx)
	for _, update := range []string{
		`UPDATE wager_transactions SET amount_minor = 1 WHERE id = $1`,
		`UPDATE wager_transactions SET kind = 'ROLLBACK' WHERE id = $1`,
		`UPDATE wager_transactions SET payload_hash = '\x02' WHERE id = $1`,
		`UPDATE wager_transactions SET reference_external_id = 'outra' WHERE id = $1`,
	} {
		_, err = mig.Exec(ctx, update, x.ID)
		requirePgError(t, err, sqlstateIntegrity, "wager_tx_immutable")
	}
}

func TestWagerTransactionCannotBeDeletedOrTruncated(t *testing.T) {
	ctx := testContext(t)
	conn := runtimeConn(t, ctx)
	w := openWallet(t, ctx, conn, 10000)
	bet := externalTx(t, w, "BET", 100)
	if err := process(t, ctx, conn, w, bet, "DEBIT"); err != nil {
		t.Fatal(err)
	}

	_, err := conn.Exec(ctx, `DELETE FROM wager_transactions WHERE id = $1`, bet.ID)
	requirePgError(t, err, sqlstatePrivilege, "")

	mig := migratorConn(t, ctx)
	_, err = mig.Exec(ctx, `DELETE FROM wager_transactions WHERE id = $1`, bet.ID)
	requirePgError(t, err, sqlstateIntegrity, "wager_tx_append_only")

	// CASCADE para passar pela checagem de FKs e chegar aos triggers.
	err = rollbackOnly(ctx, mig, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `TRUNCATE wager_transactions CASCADE`)
		return err
	})
	requirePgError(t, err, sqlstateIntegrity, "")
}

// E3: PENDING existe só dentro da transação.
func TestPendingIsNeverCommitted(t *testing.T) {
	ctx := testContext(t)
	conn := runtimeConn(t, ctx)
	w := openWallet(t, ctx, conn, 0)

	x := externalTx(t, w, "LOSS", 0)
	err := inTx(ctx, conn, func(tx pgx.Tx) error { return x.insert(ctx, tx) })
	requirePgError(t, err, sqlstateCheck, "wager_tx_pending_not_committed")

	// Mesmo fluxo, concluído antes do commit: aceito.
	y := externalTx(t, w, "LOSS", 0)
	err = inTx(ctx, conn, func(tx pgx.Tx) error {
		if err := y.insert(ctx, tx); err != nil {
			return err
		}
		return finish(ctx, tx, y.ID, "PROCESSED", nil, w.Balance)
	})
	if err != nil {
		t.Errorf("LOSS concluído na mesma transação: %v", err)
	}
}

// --- wallet_ledger_entries ---------------------------------------------------

func TestLedgerIsAppendOnly(t *testing.T) {
	ctx := testContext(t)
	conn := runtimeConn(t, ctx)
	w := openWallet(t, ctx, conn, 10000)

	for _, stmt := range []string{
		`UPDATE wallet_ledger_entries SET amount_minor = 1 WHERE wallet_id = $1`,
		`DELETE FROM wallet_ledger_entries WHERE wallet_id = $1`,
	} {
		_, err := conn.Exec(ctx, stmt, w.ID)
		requirePgError(t, err, sqlstatePrivilege, "")
	}

	mig := migratorConn(t, ctx)
	for _, stmt := range []string{
		`UPDATE wallet_ledger_entries SET created_at = now() WHERE wallet_id = $1`,
		`DELETE FROM wallet_ledger_entries WHERE wallet_id = $1`,
	} {
		_, err := mig.Exec(ctx, stmt, w.ID)
		requirePgError(t, err, sqlstateIntegrity, "ledger_append_only")
	}

	err := rollbackOnly(ctx, mig, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `TRUNCATE wallet_ledger_entries`)
		return err
	})
	requirePgError(t, err, sqlstateIntegrity, "ledger_append_only")
}

// ledgerAttempt tenta gravar uma BET de 8000 sobre uma carteira com 10000,
// com o lançamento alterado por change.
func ledgerAttempt(t *testing.T, ctx context.Context, conn *pgx.Conn, change func(e *entry)) error {
	t.Helper()
	w := openWallet(t, ctx, conn, 10000)
	bet := externalTx(t, w, "BET", 8000)
	e := entryFor(t, w, bet, "DEBIT", w.Balance)
	change(&e)
	return inTx(ctx, conn, func(tx pgx.Tx) error {
		if err := bet.insert(ctx, tx); err != nil {
			return err
		}
		if err := w.setBalance(ctx, tx, e.After); err != nil {
			return err
		}
		if err := e.insert(ctx, tx); err != nil {
			return err
		}
		return finish(ctx, tx, bet.ID, "PROCESSED", nil, e.After)
	})
}

func TestLedgerRejectsInvalidEntries(t *testing.T) {
	ctx := testContext(t)
	conn := runtimeConn(t, ctx)

	tests := []struct {
		name       string
		change     func(e *entry)
		code       string
		constraint string
	}{
		{"conta errada", func(e *entry) { e.After = 2500 }, sqlstateCheck, "ledger_balance_arithmetic"},
		{"antes diferente do depois anterior", func(e *entry) { e.Before, e.After = 9000, 1000 }, sqlstateCheck, "ledger_sequence"},
		{"salto de versão", func(e *entry) { e.Version = 3 }, sqlstateCheck, "ledger_sequence"},
		{"valor diferente da operação", func(e *entry) { e.Amount, e.After = 7000, 3000 }, sqlstateCheck, "ledger_matches_transaction"},
		{"direção errada para BET", func(e *entry) { e.Direction, e.After = "CREDIT", 18000 }, sqlstateCheck, "ledger_matches_transaction"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requirePgError(t, ledgerAttempt(t, ctx, conn, tt.change), tt.code, tt.constraint)
		})
	}
}

// Correção 4: regra do primeiro lançamento.
func TestLedgerFirstEntryRule(t *testing.T) {
	ctx := testContext(t)
	conn := runtimeConn(t, ctx)

	t.Run("OPENING precisa ser a versão 1", func(t *testing.T) {
		w := &wallet{ID: newID(t), PlayerID: newID(t), Currency: "BRL", Balance: 10000, Version: 1}
		err := inTx(ctx, conn, func(tx pgx.Tx) error {
			if err := w.insert(ctx, tx); err != nil {
				return err
			}
			opening := openingTx(t, w, 10000)
			if err := opening.insert(ctx, tx); err != nil {
				return err
			}
			e := entryFor(t, w, opening, "CREDIT", 0)
			e.Version = 2
			return e.insert(ctx, tx)
		})
		requirePgError(t, err, sqlstateCheck, "ledger_sequence")
	})

	t.Run("carteira aberta com zero: primeiro lançamento é a versão 2", func(t *testing.T) {
		w := openWallet(t, ctx, conn, 0)
		if err := process(t, ctx, conn, w, externalTx(t, w, "WIN", 500), "CREDIT"); err != nil {
			t.Fatalf("WIN na versão 2: %v", err)
		}
		if w.Balance != 500 || w.Version != 2 {
			t.Errorf("carteira = saldo %d versão %d, want 500 e 2", w.Balance, w.Version)
		}
	})

	t.Run("carteira aberta com zero: versão 1 é recusada", func(t *testing.T) {
		w := openWallet(t, ctx, conn, 0)
		win := externalTx(t, w, "WIN", 500)
		e := entryFor(t, w, win, "CREDIT", 0)
		e.Version = 1
		err := inTx(ctx, conn, func(tx pgx.Tx) error {
			if err := win.insert(ctx, tx); err != nil {
				return err
			}
			return e.insert(ctx, tx)
		})
		requirePgError(t, err, sqlstateCheck, "ledger_sequence")
	})
}

func TestLedgerOneEntryPerTransaction(t *testing.T) {
	ctx := testContext(t)
	conn := runtimeConn(t, ctx)
	w := openWallet(t, ctx, conn, 10000)
	bet := externalTx(t, w, "BET", 1000)
	if err := process(t, ctx, conn, w, bet, "DEBIT"); err != nil {
		t.Fatal(err)
	}

	// Segundo lançamento da mesma operação, com sequência correta.
	again := entryFor(t, w, bet, "DEBIT", w.Balance)
	err := inTx(ctx, conn, func(tx pgx.Tx) error {
		if err := w.setBalance(ctx, tx, again.After); err != nil {
			return err
		}
		return again.insert(ctx, tx)
	})
	requirePgError(t, err, sqlstateUnique, "ledger_wallet_transaction_key")
}

func TestLedgerEntryMustBelongToSameWallet(t *testing.T) {
	ctx := testContext(t)
	conn := runtimeConn(t, ctx)
	a := openWallet(t, ctx, conn, 10000)
	b := openWallet(t, ctx, conn, 10000)

	// Operação da carteira B lançada no extrato da carteira A.
	betB := externalTx(t, b, "BET", 1000)
	e := entryFor(t, a, betB, "DEBIT", a.Balance)
	err := inTx(ctx, conn, func(tx pgx.Tx) error {
		if err := betB.insert(ctx, tx); err != nil {
			return err
		}
		return e.insert(ctx, tx)
	})
	requirePgError(t, err, sqlstateFK, "ledger_transaction_wallet_fkey")
}

func TestLedgerRollbackDirectionIsOppositeOfOriginal(t *testing.T) {
	ctx := testContext(t)
	conn := runtimeConn(t, ctx)
	w := openWallet(t, ctx, conn, 10000)
	bet := externalTx(t, w, "BET", 1000)
	if err := process(t, ctx, conn, w, bet, "DEBIT"); err != nil {
		t.Fatal(err)
	}

	err := process(t, ctx, conn, w, reversalTx(t, w, "ROLLBACK", bet), "DEBIT")
	requirePgError(t, err, sqlstateCheck, "ledger_matches_transaction")

	if err := process(t, ctx, conn, w, reversalTx(t, w, "ROLLBACK", bet), "CREDIT"); err != nil {
		t.Errorf("ROLLBACK de BET como CREDIT: %v", err)
	}
}

// E2 (diferido): operação recusada não pode ter lançamento.
func TestLedgerRequiresProcessedTransactionAtCommit(t *testing.T) {
	ctx := testContext(t)
	conn := runtimeConn(t, ctx)
	w := openWallet(t, ctx, conn, 10000)
	bet := externalTx(t, w, "BET", 1000)
	e := entryFor(t, w, bet, "DEBIT", w.Balance)

	err := inTx(ctx, conn, func(tx pgx.Tx) error {
		if err := bet.insert(ctx, tx); err != nil {
			return err
		}
		if err := w.setBalance(ctx, tx, e.After); err != nil {
			return err
		}
		if err := e.insert(ctx, tx); err != nil {
			return err
		}
		return finish(ctx, tx, bet.ID, "REJECTED", ptr("INSUFFICIENT_FUNDS"), e.After)
	})
	requirePgError(t, err, sqlstateCheck, "ledger_transaction_processed")
}

// Fluxo completo aceito: abertura, aposta e rollback. O saldo armazenado bate
// com a soma do extrato.
func TestLedgerHappyPathReconciles(t *testing.T) {
	ctx := testContext(t)
	conn := runtimeConn(t, ctx)
	w := openWallet(t, ctx, conn, 10000)
	bet := externalTx(t, w, "BET", 8000)
	if err := process(t, ctx, conn, w, bet, "DEBIT"); err != nil {
		t.Fatalf("BET: %v", err)
	}
	if err := process(t, ctx, conn, w, reversalTx(t, w, "ROLLBACK", bet), "CREDIT"); err != nil {
		t.Fatalf("ROLLBACK: %v", err)
	}

	var stored, version, calculated, entries int64
	err := conn.QueryRow(ctx,
		`SELECT w.balance_minor, w.version,
		        COALESCE(SUM(CASE l.direction WHEN 'CREDIT' THEN l.amount_minor ELSE -l.amount_minor END), 0),
		        COUNT(l.id)
		   FROM wallets w
		   LEFT JOIN wallet_ledger_entries l ON l.wallet_id = w.id
		  WHERE w.id = $1
		  GROUP BY w.balance_minor, w.version`, w.ID).Scan(&stored, &version, &calculated, &entries)
	if err != nil {
		t.Fatal(err)
	}
	if stored != 10000 || calculated != 10000 || version != 3 || entries != 3 {
		t.Errorf("saldo %d, calculado %d, versão %d, lançamentos %d; want 10000, 10000, 3, 3",
			stored, calculated, version, entries)
	}
}

// --- inbox_messages ----------------------------------------------------------

func insertInbox(ctx context.Context, db execer, consumer, messageID string) error {
	_, err := db.Exec(ctx,
		`INSERT INTO inbox_messages (consumer_name, message_id, payload_hash, received_at)
		 VALUES ($1, $2, '\x01', now())`, consumer, messageID)
	return err
}

func TestInboxMessages(t *testing.T) {
	ctx := testContext(t)
	conn := runtimeConn(t, ctx)
	msg := "msg-" + newID(t).String()

	if err := insertInbox(ctx, conn, "wager-consumer", msg); err != nil {
		t.Fatal(err)
	}
	requirePgError(t, insertInbox(ctx, conn, "wager-consumer", msg), sqlstateUnique, "inbox_messages_pkey")

	// Outro consumidor pode registrar o mesmo messageId.
	if err := insertInbox(ctx, conn, "outro-consumer", msg); err != nil {
		t.Errorf("mesmo messageId em outro consumidor: %v", err)
	}

	// app_runtime conclui a mensagem (correção 4)...
	w := openWallet(t, ctx, conn, 10000)
	bet := externalTx(t, w, "BET", 100)
	if err := process(t, ctx, conn, w, bet, "DEBIT"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx,
		`UPDATE inbox_messages SET transaction_id = $3, processed_at = now()
		  WHERE consumer_name = $1 AND message_id = $2`, "wager-consumer", msg, bet.ID); err != nil {
		t.Errorf("concluir mensagem como app_runtime: %v", err)
	}

	// ...mas não mexe no hash; nem o dono da tabela consegue.
	_, err := conn.Exec(ctx, `UPDATE inbox_messages SET payload_hash = '\x02' WHERE message_id = $1`, msg)
	requirePgError(t, err, sqlstatePrivilege, "")
	_, err = migratorConn(t, ctx).Exec(ctx, `UPDATE inbox_messages SET payload_hash = '\x02' WHERE message_id = $1`, msg)
	requirePgError(t, err, sqlstateIntegrity, "inbox_identity_immutable")
}

// --- outbox_events -----------------------------------------------------------

func TestOutboxEvents(t *testing.T) {
	ctx := testContext(t)
	conn := runtimeConn(t, ctx)
	eventID := newID(t)

	_, err := conn.Exec(ctx,
		`INSERT INTO outbox_events (event_id, aggregate_type, aggregate_id, event_type, event_version,
		                            correlation_id, payload, occurred_at, next_attempt_at)
		 VALUES ($1, 'Wallet', $2, 'WalletBalanceChanged', 1, 'corr-1', '{"a":1}', now(), now())`,
		eventID, newID(t))
	if err != nil {
		t.Fatal(err)
	}

	// Conteúdo imutável: sem permissão para app_runtime; trigger para o dono.
	_, err = conn.Exec(ctx, `UPDATE outbox_events SET payload = '{"a":2}' WHERE event_id = $1`, eventID)
	requirePgError(t, err, sqlstatePrivilege, "")
	_, err = migratorConn(t, ctx).Exec(ctx, `UPDATE outbox_events SET payload = '{"a":2}' WHERE event_id = $1`, eventID)
	requirePgError(t, err, sqlstateIntegrity, "outbox_event_immutable")

	// Lease: dono e prazo juntos.
	_, err = conn.Exec(ctx, `UPDATE outbox_events SET locked_by = 'instancia-1' WHERE event_id = $1`, eventID)
	requirePgError(t, err, sqlstateCheck, "outbox_lease_coherent")

	// Campos de entrega mudam normalmente.
	if _, err := conn.Exec(ctx,
		`UPDATE outbox_events
		    SET locked_by = 'instancia-1', locked_until = now() + interval '30 seconds', attempts = 1
		  WHERE event_id = $1`, eventID); err != nil {
		t.Errorf("reservar evento: %v", err)
	}
	if _, err := conn.Exec(ctx,
		`UPDATE outbox_events SET published_at = now(), locked_by = NULL, locked_until = NULL
		  WHERE event_id = $1`, eventID); err != nil {
		t.Errorf("marcar como publicado: %v", err)
	}

	// Depois de publicado, nada muda.
	_, err = conn.Exec(ctx, `UPDATE outbox_events SET attempts = 2 WHERE event_id = $1`, eventID)
	requirePgError(t, err, sqlstateIntegrity, "outbox_event_published_frozen")
}
