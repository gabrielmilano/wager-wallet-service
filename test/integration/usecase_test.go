//go:build integration

package integration

import (
	"context"
	"crypto/sha256"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gabrielmilano/wager-wallet-service/internal/app/apperr"
	"github.com/gabrielmilano/wager-wallet-service/internal/app/wagering"
	"github.com/gabrielmilano/wager-wallet-service/internal/app/wallets"
	"github.com/gabrielmilano/wager-wallet-service/internal/domain/event"
	"github.com/gabrielmilano/wager-wallet-service/internal/domain/money"
	"github.com/gabrielmilano/wager-wallet-service/internal/domain/wager"
)

// --- abertura ----------------------------------------------------------------

func TestOpenWallet(t *testing.T) {
	ctx := testContext(t)
	u := newUsecases(t, ctx, 3*time.Second)

	t.Run("saldo positivo grava OPENING, lançamento e eventos no mesmo commit", func(t *testing.T) {
		w := u.open(t, ctx, "1000.00")
		if amount, version := u.balance(t, ctx, w.ID()); amount != "1000.00" || version != 1 {
			t.Errorf("saldo %s versão %d, want 1000.00 e 1", amount, version)
		}
		if n := u.count(t, ctx, `SELECT count(*) FROM wager_transactions
		                          WHERE wallet_id = $1 AND kind = 'OPENING' AND status = 'PROCESSED'`, w.ID()); n != 1 {
			t.Errorf("OPENING: %d", n)
		}
		if n := u.ledgerCount(t, ctx, w.ID()); n != 1 {
			t.Errorf("lançamentos: %d, want 1", n)
		}
		want := []string{event.TypeWagerTransactionProcessed, event.TypeWalletBalanceChanged}
		if got := u.eventTypes(t, ctx, w.ID()); !slices.Equal(got, want) {
			t.Errorf("eventos = %v, want %v", got, want)
		}
	})

	t.Run("saldo zero grava só a carteira", func(t *testing.T) {
		w := u.open(t, ctx, "0.00")
		if n := u.ledgerCount(t, ctx, w.ID()); n != 0 {
			t.Errorf("lançamentos: %d", n)
		}
		if got := u.eventTypes(t, ctx, w.ID()); len(got) != 0 {
			t.Errorf("eventos: %v", got)
		}
	})

	t.Run("mesma moeda e jogador: WALLET_ALREADY_EXISTS", func(t *testing.T) {
		w := u.open(t, ctx, "10.00")
		_, err := u.wallets.Open(ctx, wallets.OpenInput{PlayerID: w.PlayerID().String(), InitialBalance: money2(t, "5.00")})
		requireAppErr(t, err, apperr.Conflict, apperr.CodeWalletAlreadyExists)
	})

	t.Run("saldo negativo: VALIDATION_ERROR", func(t *testing.T) {
		_, err := u.wallets.Open(ctx, wallets.OpenInput{PlayerID: uuid.NewString(), InitialBalance: money2(t, "-1.00")})
		requireAppErr(t, err, apperr.Validation, apperr.CodeValidation)
	})
}

// --- operações simples -------------------------------------------------------

func TestBetWinLoss(t *testing.T) {
	ctx := testContext(t)
	u := newUsecases(t, ctx, 3*time.Second)
	w := u.open(t, ctx, "100.00")

	bet := u.mustProcess(t, ctx, op(t, w, "BET", "25.00", ""), wager.Processed)
	if bet.ResultBalance().Amount() != "75.00" {
		t.Errorf("saldo devolvido = %s, want 75.00", bet.ResultBalance().Amount())
	}
	u.mustProcess(t, ctx, op(t, w, "WIN", "10.00", ""), wager.Processed)

	// LOSS: sem movimentação, sem lançamento, versão inalterada.
	_, versionBefore := u.balance(t, ctx, w.ID())
	loss := u.mustProcess(t, ctx, op(t, w, "LOSS", "0.00", ""), wager.Processed)
	amount, version := u.balance(t, ctx, w.ID())
	if amount != "85.00" || version != versionBefore || loss.ResultBalance().Amount() != "85.00" {
		t.Errorf("após LOSS: saldo %s versão %d (antes %d)", amount, version, versionBefore)
	}
	if n := u.ledgerCount(t, ctx, w.ID()); n != 3 {
		t.Errorf("lançamentos = %d, want 3 (abertura, BET, WIN)", n)
	}
	want := []string{
		event.TypeWagerTransactionProcessed, event.TypeWalletBalanceChanged, // abertura
		event.TypeWagerTransactionProcessed, event.TypeWalletBalanceChanged, // BET
		event.TypeWagerTransactionProcessed, event.TypeWalletBalanceChanged, // WIN
		event.TypeWagerTransactionProcessed, // LOSS: sem WalletBalanceChanged
	}
	if got := u.eventTypes(t, ctx, w.ID()); !slices.Equal(got, want) {
		t.Errorf("eventos = %v\nwant %v", got, want)
	}
	u.reconcile(t, ctx, w.ID())
}

func TestBetWithoutFundsIsRejected(t *testing.T) {
	ctx := testContext(t)
	u := newUsecases(t, ctx, 3*time.Second)
	w := u.open(t, ctx, "10.00")

	tx := u.mustProcess(t, ctx, op(t, w, "BET", "10.01", ""), wager.Rejected)
	if tx.FailureCode() != wager.InsufficientFunds || tx.ResultBalance().Amount() != "10.00" {
		t.Errorf("rejeição = %s, saldo %s", tx.FailureCode(), tx.ResultBalance().Amount())
	}
	if amount, version := u.balance(t, ctx, w.ID()); amount != "10.00" || version != 1 {
		t.Errorf("carteira mudou: %s v%d", amount, version)
	}
	if n := u.ledgerCount(t, ctx, w.ID()); n != 1 {
		t.Errorf("lançamentos = %d, want só a abertura", n)
	}
	if got := u.eventTypes(t, ctx, w.ID()); got[len(got)-1] != event.TypeWagerTransactionRejected {
		t.Errorf("último evento = %v", got)
	}
}

// --- idempotência ------------------------------------------------------------

func TestIdempotency(t *testing.T) {
	ctx := testContext(t)
	u := newUsecases(t, ctx, 3*time.Second)
	w := u.open(t, ctx, "100.00")

	in := op(t, w, "BET", "30.00", "")
	first := u.mustProcess(t, ctx, in, wager.Processed)

	// Outra movimentação depois: o replay devolve o saldo do processamento original.
	u.mustProcess(t, ctx, op(t, w, "BET", "5.00", ""), wager.Processed)

	t.Run("mesma chave e conteúdo: replay do resultado original", func(t *testing.T) {
		res, err := u.process(t, ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		if !res.Replay || res.Transaction.ID() != first.ID() || res.Transaction.ResultBalance().Amount() != "70.00" {
			t.Errorf("replay = %v, id %s, saldo %s", res.Replay, res.Transaction.ID(), res.Transaction.ResultBalance().Amount())
		}
		if amount, _ := u.balance(t, ctx, w.ID()); amount != "65.00" {
			t.Errorf("replay movimentou: saldo %s", amount)
		}
	})

	t.Run("mesma chave com conteúdo diferente: IDEMPOTENCY_CONFLICT", func(t *testing.T) {
		changed := in
		changed.Money = money2(t, "31.00")
		cmd, _ := wagering.NewCommand(changed)
		_, err := u.wagers.Process(ctx, cmd, keyFor(in))
		requireAppErr(t, err, apperr.Conflict, apperr.CodeIdempotencyConflict)
	})

	t.Run("mesmo id externo com outra chave: IDEMPOTENCY_CONFLICT", func(t *testing.T) {
		cmd, _ := wagering.NewCommand(in)
		_, err := u.wagers.Process(ctx, cmd, wagering.Metadata{IdempotencyKey: "outra-chave-" + uuid.NewString()})
		requireAppErr(t, err, apperr.Conflict, apperr.CodeIdempotencyConflict)
	})

	t.Run("rejeição também é reproduzida", func(t *testing.T) {
		big := op(t, w, "BET", "1000.00", "")
		rejected := u.mustProcess(t, ctx, big, wager.Rejected)
		res, err := u.process(t, ctx, big)
		if err != nil || !res.Replay || res.Transaction.ID() != rejected.ID() || res.Transaction.Status() != wager.Rejected {
			t.Errorf("replay da rejeição = %+v, %v", res, err)
		}
	})

	if n := u.count(t, ctx, `SELECT count(*) FROM wager_transactions WHERE provider_id = $1 AND external_transaction_id = $2`,
		in.ProviderID, in.ExternalTransactionID); n != 1 {
		t.Errorf("operações gravadas = %d, want 1", n)
	}
	u.reconcile(t, ctx, w.ID())
}

func TestWalletErrorsRecordNothing(t *testing.T) {
	ctx := testContext(t)
	u := newUsecases(t, ctx, 3*time.Second)
	w := u.open(t, ctx, "100.00")

	missing := op(t, w, "BET", "1.00", "")
	missing.WalletID = uuid.NewString()
	_, err := u.process(t, ctx, missing)
	requireAppErr(t, err, apperr.NotFound, apperr.CodeWalletNotFound)

	otherPlayer := op(t, w, "BET", "1.00", "")
	otherPlayer.PlayerID = uuid.NewString()
	_, err = u.process(t, ctx, otherPlayer)
	requireAppErr(t, err, apperr.Validation, apperr.CodeWalletMismatch)

	usd := op(t, w, "BET", "1.00", "")
	usd.Money, _ = moneyUSD("1.00")
	_, err = u.process(t, ctx, usd)
	requireAppErr(t, err, apperr.Validation, apperr.CodeWalletMismatch)

	for _, in := range []wagering.Input{missing, otherPlayer, usd} {
		if n := u.count(t, ctx, `SELECT count(*) FROM wager_transactions WHERE external_transaction_id = $1`,
			in.ExternalTransactionID); n != 0 {
			t.Errorf("%s gravou %d operações", in.ExternalTransactionID, n)
		}
	}
}

// --- reversões e referências -------------------------------------------------

func TestRefundAndRollback(t *testing.T) {
	ctx := testContext(t)
	u := newUsecases(t, ctx, 3*time.Second)

	t.Run("REFUND devolve a BET; segunda reversão é recusada", func(t *testing.T) {
		w := u.open(t, ctx, "100.00")
		bet := op(t, w, "BET", "40.00", "")
		betTx := u.mustProcess(t, ctx, bet, wager.Processed)

		refund := u.mustProcess(t, ctx, op(t, w, "REFUND", "40.00", bet.ExternalTransactionID), wager.Processed)
		if refund.ReferenceTxID() == nil || *refund.ReferenceTxID() != betTx.ID() {
			t.Errorf("referência resolvida = %v, want %s", refund.ReferenceTxID(), betTx.ID())
		}
		if amount, _ := u.balance(t, ctx, w.ID()); amount != "100.00" {
			t.Errorf("saldo após REFUND = %s", amount)
		}

		again := u.mustProcess(t, ctx, op(t, w, "ROLLBACK", "40.00", bet.ExternalTransactionID), wager.Rejected)
		if again.FailureCode() != wager.ReferenceAlreadyReversed {
			t.Errorf("código = %s", again.FailureCode())
		}
		u.reconcile(t, ctx, w.ID())
	})

	t.Run("ROLLBACK de REFUND debita de novo", func(t *testing.T) {
		w := u.open(t, ctx, "100.00")
		bet := op(t, w, "BET", "40.00", "")
		u.mustProcess(t, ctx, bet, wager.Processed)
		refund := op(t, w, "REFUND", "40.00", bet.ExternalTransactionID)
		u.mustProcess(t, ctx, refund, wager.Processed)
		u.mustProcess(t, ctx, op(t, w, "ROLLBACK", "40.00", refund.ExternalTransactionID), wager.Processed)
		if amount, version := u.balance(t, ctx, w.ID()); amount != "60.00" || version != 4 {
			t.Errorf("saldo %s versão %d, want 60.00 e 4", amount, version)
		}
		u.reconcile(t, ctx, w.ID())
	})

	t.Run("ROLLBACK de WIN sem saldo: INSUFFICIENT_FUNDS_FOR_REVERSAL", func(t *testing.T) {
		w := u.open(t, ctx, "10.00")
		win := op(t, w, "WIN", "50.00", "")
		u.mustProcess(t, ctx, win, wager.Processed)
		u.mustProcess(t, ctx, op(t, w, "BET", "55.00", ""), wager.Processed) // sobra 5.00
		rb := u.mustProcess(t, ctx, op(t, w, "ROLLBACK", "50.00", win.ExternalTransactionID), wager.Rejected)
		if rb.FailureCode() != wager.InsufficientFundsForReversal {
			t.Errorf("código = %s, want %s", rb.FailureCode(), wager.InsufficientFundsForReversal)
		}
		if amount, _ := u.balance(t, ctx, w.ID()); amount != "5.00" {
			t.Errorf("saldo = %s", amount)
		}
	})

	t.Run("rejeições de referência", func(t *testing.T) {
		w := u.open(t, ctx, "100.00")
		bet := op(t, w, "BET", "40.00", "")
		u.mustProcess(t, ctx, bet, wager.Processed)
		win := op(t, w, "WIN", "10.00", "")
		u.mustProcess(t, ctx, win, wager.Processed)
		rejected := op(t, w, "BET", "999.00", "")
		u.mustProcess(t, ctx, rejected, wager.Rejected)

		otherRound := op(t, w, "REFUND", "40.00", bet.ExternalTransactionID)
		otherRound.RoundID = "round-2"

		cases := map[wager.FailureCode]wagering.Input{
			wager.ReferenceAmountMismatch:  op(t, w, "REFUND", "39.99", bet.ExternalTransactionID),
			wager.ReferenceContextMismatch: otherRound,
			wager.InvalidReferenceKind:     op(t, w, "REFUND", "10.00", win.ExternalTransactionID),
			wager.ReferenceNotProcessed:    op(t, w, "REFUND", "999.00", rejected.ExternalTransactionID),
		}
		for code, in := range cases {
			tx := u.mustProcess(t, ctx, in, wager.Rejected)
			if tx.FailureCode() != code {
				t.Errorf("%s: código = %s", code, tx.FailureCode())
			}
		}
		u.reconcile(t, ctx, w.ID())
	})

	t.Run("WIN com referência à BET da rodada", func(t *testing.T) {
		w := u.open(t, ctx, "100.00")
		bet := op(t, w, "BET", "10.00", "")
		betTx := u.mustProcess(t, ctx, bet, wager.Processed)
		win := u.mustProcess(t, ctx, op(t, w, "WIN", "35.00", bet.ExternalTransactionID), wager.Processed)
		if win.ReferenceTxID() == nil || *win.ReferenceTxID() != betTx.ID() {
			t.Errorf("WIN sem a referência resolvida")
		}
		if amount, _ := u.balance(t, ctx, w.ID()); amount != "125.00" {
			t.Errorf("saldo = %s", amount)
		}
	})
}

func TestReversalBeforeReferenceWaits(t *testing.T) {
	ctx := testContext(t)
	u := newUsecases(t, ctx, 3*time.Second)
	w := u.open(t, ctx, "100.00")

	refund := op(t, w, "REFUND", "20.00", "bet-que-ainda-nao-chegou-"+uuid.NewString())
	tx := u.mustProcess(t, ctx, refund, wager.PendingReference)
	if tx.NextAttemptAt() == nil || tx.ExpiresAt() == nil || tx.ExpiresAt().Sub(tx.CreatedAt()) != wagering.ReferenceTTL {
		t.Errorf("agenda incorreta: próxima %v, expira %v", tx.NextAttemptAt(), tx.ExpiresAt())
	}
	if amount, version := u.balance(t, ctx, w.ID()); amount != "100.00" || version != 1 {
		t.Errorf("carteira mudou: %s v%d", amount, version)
	}
	got := u.eventTypes(t, ctx, w.ID())
	if got[len(got)-1] != event.TypeWagerTransactionPendingReference {
		t.Errorf("último evento = %v", got)
	}

	// Replay de uma pendência devolve a pendência.
	res, err := u.process(t, ctx, refund)
	if err != nil || !res.Replay || res.Transaction.Status() != wager.PendingReference {
		t.Errorf("replay da pendência = %+v, %v", res, err)
	}
}

// --- SQS: inbox na mesma transação -------------------------------------------

func TestProcessFromMessage(t *testing.T) {
	ctx := testContext(t)
	u := newUsecases(t, ctx, 3*time.Second)
	w := u.open(t, ctx, "100.00")

	in := op(t, w, "BET", "10.00", "")
	cmd, err := wagering.NewCommand(in)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"messageId":"` + uuid.NewString() + `"}`)
	sum := sha256.Sum256(body)
	msg := wagering.InboundMessage{ConsumerName: "wager-consumer", MessageID: "msg-" + uuid.NewString(), PayloadHash: sum[:]}

	first, err := u.wagers.ProcessFromMessage(ctx, cmd, keyFor(in), msg)
	if err != nil || first.Replay || first.Transaction.Status() != wager.Processed {
		t.Fatalf("primeira entrega = %+v, %v", first, err)
	}
	if n := u.count(t, ctx, `SELECT count(*) FROM inbox_messages
	                          WHERE message_id = $1 AND processed_at IS NOT NULL AND transaction_id = $2`,
		msg.MessageID, first.Transaction.ID()); n != 1 {
		t.Errorf("inbox concluída = %d", n)
	}

	t.Run("reentrega da mesma mensagem: replay", func(t *testing.T) {
		again, err := u.wagers.ProcessFromMessage(ctx, cmd, keyFor(in), msg)
		if err != nil || !again.Replay || again.Transaction.ID() != first.Transaction.ID() {
			t.Errorf("reentrega = %+v, %v", again, err)
		}
	})

	t.Run("mesmo messageId com outro conteúdo: MESSAGE_CONFLICT", func(t *testing.T) {
		other := msg
		other.PayloadHash = []byte{9, 9, 9}
		_, err := u.wagers.ProcessFromMessage(ctx, cmd, keyFor(in), other)
		requireAppErr(t, err, apperr.Conflict, apperr.CodeMessageConflict)
	})

	t.Run("mesma operação por HTTP depois do SQS: replay", func(t *testing.T) {
		res, err := u.wagers.Process(ctx, cmd, keyFor(in))
		if err != nil || !res.Replay || res.Transaction.ID() != first.Transaction.ID() {
			t.Errorf("HTTP = %+v, %v", res, err)
		}
	})

	t.Run("erro corrigível não grava a inbox", func(t *testing.T) {
		bad := op(t, w, "BET", "1.00", "")
		bad.WalletID = uuid.NewString()
		badCmd, _ := wagering.NewCommand(bad)
		badMsg := wagering.InboundMessage{ConsumerName: "wager-consumer", MessageID: "msg-" + uuid.NewString(), PayloadHash: []byte{1}}
		_, err := u.wagers.ProcessFromMessage(ctx, badCmd, keyFor(bad), badMsg)
		requireAppErr(t, err, apperr.NotFound, apperr.CodeWalletNotFound)
		if n := u.count(t, ctx, `SELECT count(*) FROM inbox_messages WHERE message_id = $1`, badMsg.MessageID); n != 0 {
			t.Errorf("inbox gravada mesmo com erro: %d", n)
		}
	})

	if amount, _ := u.balance(t, ctx, w.ID()); amount != "90.00" {
		t.Errorf("saldo = %s, want 90.00 (um único débito)", amount)
	}
}

// --- concorrência (uma instância; a Fase 12 repete com três) ------------------

func TestConcurrentBetsOnSameWallet(t *testing.T) {
	ctx := testContext(t)
	u := newUsecases(t, ctx, 3*time.Second)
	w := u.open(t, ctx, "100.00")

	// Duas apostas distintas de 80.00 ao mesmo tempo sobre 100.00.
	results := make([]*wager.Transaction, 2)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range results {
		in := op(t, w, "BET", "80.00", "")
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			res, err := u.process(t, ctx, in)
			if err != nil {
				t.Errorf("aposta %d: %v", i, err)
				return
			}
			results[i] = res.Transaction
		}()
	}
	close(start)
	wg.Wait()
	if t.Failed() {
		return
	}

	statuses := []wager.Status{results[0].Status(), results[1].Status()}
	slices.Sort(statuses)
	if !slices.Equal(statuses, []wager.Status{wager.Processed, wager.Rejected}) {
		t.Errorf("estados = %v, want um PROCESSED e um REJECTED", statuses)
	}
	if amount, _ := u.balance(t, ctx, w.ID()); amount != "20.00" {
		t.Errorf("saldo = %s, want 20.00", amount)
	}
	if n := u.count(t, ctx, `SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id = $1 AND direction = 'DEBIT'`, w.ID()); n != 1 {
		t.Errorf("débitos = %d, want 1", n)
	}
	u.reconcile(t, ctx, w.ID())
}

func TestSameBetFiftyTimesInParallel(t *testing.T) {
	ctx := testContext(t)
	u := newUsecases(t, ctx, 3*time.Second)
	w := u.open(t, ctx, "100.00")
	in := op(t, w, "BET", "25.00", "")

	var wg sync.WaitGroup
	var mu sync.Mutex
	ids := map[uuid.UUID]int{}
	replays := 0
	start := make(chan struct{})
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			res, err := u.process(t, ctx, in)
			if err != nil {
				t.Errorf("envio: %v", err)
				return
			}
			mu.Lock()
			ids[res.Transaction.ID()]++
			if res.Replay {
				replays++
			}
			mu.Unlock()
		}()
	}
	close(start)
	wg.Wait()

	if len(ids) != 1 || replays != 49 {
		t.Errorf("operações distintas = %d, replays = %d; want 1 e 49", len(ids), replays)
	}
	if amount, _ := u.balance(t, ctx, w.ID()); amount != "75.00" {
		t.Errorf("saldo = %s, want 75.00", amount)
	}
	if n := u.count(t, ctx, `SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id = $1 AND direction = 'DEBIT'`, w.ID()); n != 1 {
		t.Errorf("débitos = %d, want 1", n)
	}
}

func TestDifferentWalletsProceedInParallel(t *testing.T) {
	ctx := testContext(t)
	u := newUsecases(t, ctx, 3*time.Second)
	a := u.open(t, ctx, "100.00")
	b := u.open(t, ctx, "100.00")

	// Segura o lock da carteira A numa transação aberta: B não pode esperar.
	holder, err := u.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx)
	if _, err := holder.Exec(ctx, `SELECT 1 FROM wallets WHERE id = $1 FOR UPDATE`, a.ID()); err != nil {
		t.Fatal(err)
	}

	started := time.Now()
	u.mustProcess(t, ctx, op(t, b, "BET", "10.00", ""), wager.Processed)
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Errorf("carteira B esperou %v pelo lock da carteira A (lock global?)", elapsed)
	}
}

func TestLockTimeoutIsTransient(t *testing.T) {
	ctx := testContext(t)
	u := newUsecases(t, ctx, 200*time.Millisecond)
	w := u.open(t, ctx, "100.00")

	holder, err := u.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx)
	if _, err := holder.Exec(ctx, `SELECT 1 FROM wallets WHERE id = $1 FOR UPDATE`, w.ID()); err != nil {
		t.Fatal(err)
	}

	in := op(t, w, "BET", "10.00", "")
	_, err = u.process(t, ctx, in)
	if !isUnavailable(err) {
		t.Fatalf("erro = %v, want Unavailable (lock_timeout)", err)
	}
	// Nada foi gravado: a mesma operação pode ser repetida depois.
	if n := u.count(t, ctx, `SELECT count(*) FROM wager_transactions WHERE external_transaction_id = $1`,
		in.ExternalTransactionID); n != 0 {
		t.Errorf("operação gravada apesar do timeout: %d", n)
	}
	_ = holder.Rollback(ctx)
	u.mustProcess(t, ctx, in, wager.Processed)
}

// --- consultas -----------------------------------------------------------------

func TestLedgerPagination(t *testing.T) {
	ctx := testContext(t)
	u := newUsecases(t, ctx, 3*time.Second)
	w := u.open(t, ctx, "100.00")
	for range 4 {
		u.mustProcess(t, ctx, op(t, w, "BET", "1.00", ""), wager.Processed)
	}

	var versions []int64
	cursor := ""
	for pages := 0; ; pages++ {
		page, err := u.wallets.Ledger(ctx, w.ID(), cursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range page.Entries {
			versions = append(versions, e.WalletVersion())
		}
		if page.NextCursor == "" || pages > 5 {
			break
		}
		cursor = page.NextCursor
	}
	if !slices.Equal(versions, []int64{1, 2, 3, 4, 5}) {
		t.Errorf("versões = %v, want 1..5 sem repetição", versions)
	}

	_, err := u.wallets.Ledger(ctx, w.ID(), "cursor-invalido", 2)
	requireAppErr(t, err, apperr.Validation, apperr.CodeValidation)
	_, err = u.wallets.Ledger(ctx, uuid.New(), "", 2)
	requireAppErr(t, err, apperr.NotFound, apperr.CodeWalletNotFound)
}

func TestGetTransaction(t *testing.T) {
	ctx := testContext(t)
	u := newUsecases(t, ctx, 3*time.Second)
	w := u.open(t, ctx, "100.00")
	in := op(t, w, "BET", "10.00", "")
	tx := u.mustProcess(t, ctx, in, wager.Processed)

	byID, err := u.wagers.GetTransaction(ctx, tx.ID())
	if err != nil || byID.ExternalID() != in.ExternalTransactionID {
		t.Errorf("GetTransaction = %v, %v", byID, err)
	}
	byExt, err := u.wagers.GetByExternalID(ctx, in.ProviderID, in.ExternalTransactionID)
	if err != nil || byExt.ID() != tx.ID() {
		t.Errorf("GetByExternalID = %v, %v", byExt, err)
	}
	_, err = u.wagers.GetTransaction(ctx, uuid.New())
	requireAppErr(t, err, apperr.NotFound, apperr.CodeTransactionNotFound)
}

func moneyUSD(amount string) (money.Money, error) { return money.Parse(amount, "USD") }

// cancelado: garante que um contexto cancelado não deixa transação aberta.
func TestCanceledContextLeavesNothing(t *testing.T) {
	ctx := testContext(t)
	u := newUsecases(t, ctx, 3*time.Second)
	w := u.open(t, ctx, "100.00")
	in := op(t, w, "BET", "10.00", "")

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := u.process(t, canceled, in); err == nil {
		t.Fatal("esperado erro com contexto cancelado")
	}
	if n := u.count(t, ctx, `SELECT count(*) FROM wager_transactions WHERE external_transaction_id = $1`,
		in.ExternalTransactionID); n != 0 {
		t.Errorf("gravou com contexto cancelado: %d", n)
	}
}
