package wager

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gabrielmilano/wager-wallet-service/internal/domain/money"
	"github.com/gabrielmilano/wager-wallet-service/internal/domain/wallet"
)

var now = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

func amount(t *testing.T, s string) money.Money {
	t.Helper()
	m, err := money.Parse(s, "BRL")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

var (
	walletID = uuid.New()
	playerID = uuid.New()
)

func params(t *testing.T, kind Kind, value string, ref string) ExternalParams {
	t.Helper()
	return ExternalParams{
		ID: uuid.New(), WalletID: walletID, PlayerID: playerID,
		ProviderID: "provider-a", ExternalID: "ext-" + uuid.NewString(), IdempotencyKey: "key-" + uuid.NewString(),
		PayloadHash: []byte{1}, RoundID: "round-1", GameID: "game-1",
		Kind: kind, Money: amount(t, value), ReferenceExternalID: ref, Now: now,
	}
}

func external(t *testing.T, kind Kind, value, ref string) *Transaction {
	t.Helper()
	tx, err := NewExternal(params(t, kind, value, ref))
	if err != nil {
		t.Fatalf("NewExternal(%s %s): %v", kind, value, err)
	}
	return tx
}

func TestParseExternalKind(t *testing.T) {
	for _, k := range []string{"BET", "WIN", "LOSS", "REFUND", "ROLLBACK"} {
		if _, err := ParseExternalKind(k); err != nil {
			t.Errorf("%s: %v", k, err)
		}
	}
	for _, k := range []string{"OPENING", "bet", "", "CASHOUT"} {
		if _, err := ParseExternalKind(k); !errors.Is(err, ErrInvalidKind) {
			t.Errorf("%q: erro = %v, want ErrInvalidKind", k, err)
		}
	}
}

// Política de valores por tipo: zero só em LOSS; nenhum tipo aceita negativo.
func TestZeroValuePolicyPerKind(t *testing.T) {
	tests := []struct {
		kind    Kind
		value   string
		ref     string
		wantErr bool
	}{
		{Bet, "25.00", "", false},
		{Bet, "0.00", "", true},
		{Bet, "-1.00", "", true},
		{Win, "10.00", "", false},
		{Win, "0.00", "", true},
		{Loss, "0.00", "", false},
		{Loss, "0.01", "", true},
		{Loss, "-0.01", "", true},
		{Refund, "25.00", "bet-1", false},
		{Refund, "0.00", "bet-1", true},
		{Rollback, "25.00", "bet-1", false},
		{Rollback, "0.00", "bet-1", true},
	}
	for _, tt := range tests {
		_, err := NewExternal(params(t, tt.kind, tt.value, tt.ref))
		if (err != nil) != tt.wantErr {
			t.Errorf("%s %s: erro = %v, wantErr %v", tt.kind, tt.value, err, tt.wantErr)
		}
		if err != nil && !errors.Is(err, ErrInvalidTransaction) {
			t.Errorf("%s %s: erro deveria ser ErrInvalidTransaction: %v", tt.kind, tt.value, err)
		}
	}
}

func TestReferencePolicyPerKind(t *testing.T) {
	tests := []struct {
		kind    Kind
		value   string
		ref     string
		wantErr bool
	}{
		{Refund, "1.00", "", true},   // obrigatória
		{Rollback, "1.00", "", true}, // obrigatória
		{Win, "1.00", "", false},     // opcional
		{Win, "1.00", "bet-1", false},
		{Bet, "1.00", "x", true},  // não aceita
		{Loss, "0.00", "x", true}, // não aceita
	}
	for _, tt := range tests {
		_, err := NewExternal(params(t, tt.kind, tt.value, tt.ref))
		if (err != nil) != tt.wantErr {
			t.Errorf("%s ref=%q: erro = %v, wantErr %v", tt.kind, tt.ref, err, tt.wantErr)
		}
	}
}

func TestNewExternalRejectsOpeningAndMissingFields(t *testing.T) {
	p := params(t, Bet, "1.00", "")
	p.Kind = Opening
	if _, err := NewExternal(p); !errors.Is(err, ErrInvalidKind) {
		t.Errorf("OPENING externo: %v", err)
	}
	for name, change := range map[string]func(p *ExternalParams){
		"sem provedor":  func(p *ExternalParams) { p.ProviderID = "" },
		"sem id ext":    func(p *ExternalParams) { p.ExternalID = "" },
		"sem chave":     func(p *ExternalParams) { p.IdempotencyKey = "" },
		"sem hash":      func(p *ExternalParams) { p.PayloadHash = nil },
		"sem rodada":    func(p *ExternalParams) { p.RoundID = "" },
		"sem jogo":      func(p *ExternalParams) { p.GameID = "" },
		"sem carteira":  func(p *ExternalParams) { p.WalletID = uuid.Nil },
		"money ausente": func(p *ExternalParams) { p.Money = money.Money{} },
	} {
		p := params(t, Bet, "1.00", "")
		change(&p)
		if _, err := NewExternal(p); !errors.Is(err, ErrInvalidTransaction) {
			t.Errorf("%s: erro = %v, want ErrInvalidTransaction", name, err)
		}
	}
}

func TestNewOpening(t *testing.T) {
	tx, err := NewOpening(uuid.New(), walletID, playerID, amount(t, "1000.00"), now)
	if err != nil {
		t.Fatal(err)
	}
	if tx.Origin() != Internal || tx.Kind() != Opening || tx.Status() != Processed ||
		tx.ProviderID() != "" || tx.IdempotencyKey() != "" || tx.PayloadHash() != nil ||
		tx.RoundID() != "" || tx.ReferenceExternalID() != "" || tx.CompletedAt() == nil ||
		tx.ResultBalance() == nil || tx.ResultBalance().Amount() != "1000.00" {
		t.Errorf("OPENING com metadados incorretos: %+v", tx)
	}
	if _, err := NewOpening(uuid.New(), walletID, playerID, amount(t, "0.00"), now); !errors.Is(err, ErrInvalidTransaction) {
		t.Errorf("OPENING com zero: %v", err)
	}
}

func TestStateMachine(t *testing.T) {
	later := now.Add(time.Second)

	t.Run("PENDING -> PROCESSED e terminal congelado", func(t *testing.T) {
		tx := external(t, Bet, "10.00", "")
		if err := tx.Process(amount(t, "90.00"), nil, later); err != nil {
			t.Fatal(err)
		}
		if tx.Status() != Processed || tx.CompletedAt() == nil || tx.ResultBalance().Amount() != "90.00" {
			t.Errorf("estado = %s", tx.Status())
		}
		for name, err := range map[string]error{
			"process": tx.Process(amount(t, "1.00"), nil, later),
			"reject":  tx.Reject(InsufficientFunds, amount(t, "1.00"), nil, later),
			"wait":    tx.WaitForReference(later, later, later),
			"retry":   tx.ScheduleRetry(later, later),
			"fail":    tx.Fail("X", later),
		} {
			if !errors.Is(err, ErrInvalidTransition) {
				t.Errorf("%s depois de PROCESSED: %v", name, err)
			}
		}
	})

	t.Run("PENDING -> REJECTED guarda código e saldo", func(t *testing.T) {
		tx := external(t, Bet, "10.00", "")
		if err := tx.Reject(InsufficientFunds, amount(t, "5.00"), nil, later); err != nil {
			t.Fatal(err)
		}
		if tx.Status() != Rejected || tx.FailureCode() != InsufficientFunds || tx.ResultBalance().Amount() != "5.00" {
			t.Errorf("rejeição incorreta: %s %s", tx.Status(), tx.FailureCode())
		}
		if err := tx.Reject("", amount(t, "5.00"), nil, later); !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("segunda rejeição: %v", err)
		}
	})

	t.Run("PENDING -> PENDING_REFERENCE -> retry -> PROCESSED", func(t *testing.T) {
		tx := external(t, Refund, "10.00", "bet-1")
		if err := tx.WaitForReference(later, now.Add(time.Hour), later); err != nil {
			t.Fatal(err)
		}
		if tx.Status() != PendingReference || tx.NextAttemptAt() == nil || tx.ExpiresAt() == nil {
			t.Fatalf("espera incorreta: %s", tx.Status())
		}
		if err := tx.ScheduleRetry(later.Add(2*time.Second), later); err != nil || tx.Attempts() != 1 {
			t.Fatalf("retry: %v, tentativas %d", err, tx.Attempts())
		}
		ref := uuid.New()
		if err := tx.Process(amount(t, "20.00"), &ref, later); err != nil {
			t.Fatal(err)
		}
		if tx.ReferenceTxID() == nil || *tx.ReferenceTxID() != ref || tx.NextAttemptAt() != nil {
			t.Errorf("referência não registrada ou agenda não limpa")
		}
	})

	t.Run("FAILED só a partir de PENDING_REFERENCE", func(t *testing.T) {
		tx := external(t, Refund, "10.00", "bet-1")
		if err := tx.Fail("PERMANENT", later); !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("PENDING -> FAILED: %v", err)
		}
		_ = tx.WaitForReference(later, later, later)
		if err := tx.Fail("PERMANENT", later); err != nil || tx.Status() != Failed {
			t.Errorf("PENDING_REFERENCE -> FAILED: %v", err)
		}
	})

	t.Run("BET não espera referência", func(t *testing.T) {
		tx := external(t, Bet, "10.00", "")
		if err := tx.WaitForReference(later, later, later); !errors.Is(err, ErrInvalidTransaction) {
			t.Errorf("BET esperando referência: %v", err)
		}
	})
}

func TestRehydrateDoesNotTransition(t *testing.T) {
	balance := amount(t, "10.00")
	tx, err := Rehydrate(RehydrateParams{
		ID: uuid.New(), Origin: External, Kind: Bet, Status: Rejected, WalletID: walletID, PlayerID: playerID,
		Money: amount(t, "20.00"), FailureCode: InsufficientFunds, ResultBalance: &balance,
		CreatedAt: now, UpdatedAt: now, CompletedAt: &now,
	})
	if err != nil || tx.Status() != Rejected || tx.FailureCode() != InsufficientFunds {
		t.Fatalf("Rehydrate = %v, %v", tx, err)
	}
	if _, err := Rehydrate(RehydrateParams{ID: uuid.New(), WalletID: walletID, Money: balance, Status: Rejected}); !errors.Is(err, ErrInvalidTransaction) {
		t.Errorf("REJECTED sem código: %v", err)
	}
}

func TestInsufficientFundsCode(t *testing.T) {
	if InsufficientFundsCode(Bet) != InsufficientFunds || InsufficientFundsCode(Rollback) != InsufficientFundsForReversal {
		t.Error("códigos de saldo insuficiente devem diferir entre aposta e reversão")
	}
}

// --- regras de referência ----------------------------------------------------

func processed(t *testing.T, tx *Transaction) *Transaction {
	t.Helper()
	if err := tx.Process(amount(t, "100.00"), nil, now); err != nil {
		t.Fatal(err)
	}
	return tx
}

func TestResolveReference(t *testing.T) {
	bet := processed(t, external(t, Bet, "25.00", ""))
	win := processed(t, external(t, Win, "40.00", ""))
	loss := processed(t, external(t, Loss, "0.00", ""))
	refund := processed(t, external(t, Refund, "25.00", bet.ExternalID()))
	rollback := processed(t, external(t, Rollback, "25.00", bet.ExternalID()))

	rejectedBet := external(t, Bet, "25.00", "")
	_ = rejectedBet.Reject(InsufficientFunds, amount(t, "0.00"), nil, now)
	waitingRefund := external(t, Refund, "25.00", "ainda-nao")
	_ = waitingRefund.WaitForReference(now, now.Add(time.Hour), now)

	otherRound := external(t, Bet, "25.00", "")
	otherRound.roundID = "round-2"
	_ = otherRound.Process(amount(t, "1.00"), nil, now)

	tests := []struct {
		name      string
		op        *Transaction
		original  *Transaction
		reversed  bool
		outcome   Outcome
		code      FailureCode
		direction wallet.Direction
	}{
		{"original não chegou", external(t, Refund, "25.00", "x"), nil, false, Wait, "", ""},
		{"REFUND de BET", external(t, Refund, "25.00", "x"), bet, false, Proceed, "", wallet.Credit},
		{"ROLLBACK de BET", external(t, Rollback, "25.00", "x"), bet, false, Proceed, "", wallet.Credit},
		{"ROLLBACK de WIN", external(t, Rollback, "40.00", "x"), win, false, Proceed, "", wallet.Debit},
		{"ROLLBACK de REFUND", external(t, Rollback, "25.00", "x"), refund, false, Proceed, "", wallet.Debit},
		{"WIN de BET (valor livre)", external(t, Win, "99.00", "x"), bet, false, Proceed, "", wallet.Credit},
		{"REFUND de WIN", external(t, Refund, "40.00", "x"), win, false, Reject, InvalidReferenceKind, ""},
		{"REFUND de LOSS", external(t, Refund, "25.00", "x"), loss, false, Reject, InvalidReferenceKind, ""},
		{"ROLLBACK de ROLLBACK", external(t, Rollback, "25.00", "x"), rollback, false, Reject, InvalidReferenceKind, ""},
		{"ROLLBACK de LOSS", external(t, Rollback, "25.00", "x"), loss, false, Reject, InvalidReferenceKind, ""},
		{"WIN de WIN", external(t, Win, "40.00", "x"), win, false, Reject, InvalidReferenceKind, ""},
		{"rodada diferente", external(t, Refund, "25.00", "x"), otherRound, false, Reject, ReferenceContextMismatch, ""},
		{"original pendente", external(t, Rollback, "25.00", "x"), waitingRefund, false, Wait, "", ""},
		{"original rejeitada", external(t, Refund, "25.00", "x"), rejectedBet, false, Reject, ReferenceNotProcessed, ""},
		{"valor diferente", external(t, Refund, "20.00", "x"), bet, false, Reject, ReferenceAmountMismatch, ""},
		{"já revertida", external(t, Rollback, "25.00", "x"), bet, true, Reject, ReferenceAlreadyReversed, ""},
		{"WIN ignora reversão", external(t, Win, "10.00", "x"), bet, true, Proceed, "", wallet.Credit},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ResolveReference(tt.op, tt.original, tt.reversed)
			if got.Outcome != tt.outcome || got.Code != tt.code || got.Direction != tt.direction {
				t.Errorf("= %+v, want outcome %d code %q direction %q", got, tt.outcome, tt.code, tt.direction)
			}
			if tt.original != nil && got.Outcome != Wait && got.ReferenceID != tt.original.ID() {
				t.Errorf("ReferenceID = %s, want %s", got.ReferenceID, tt.original.ID())
			}
		})
	}
}

func TestResolveReferenceContextMismatch(t *testing.T) {
	bet := processed(t, external(t, Bet, "25.00", ""))
	for name, change := range map[string]func(op *Transaction){
		"jogador":  func(op *Transaction) { op.playerID = uuid.New() },
		"carteira": func(op *Transaction) { op.walletID = uuid.New() },
	} {
		op := external(t, Refund, "25.00", bet.ExternalID())
		change(op)
		if got := ResolveReference(op, bet, false); got.Code != ReferenceContextMismatch {
			t.Errorf("%s diferente: %+v", name, got)
		}
	}
}

func TestDirectionWithoutReference(t *testing.T) {
	if d, ok := DirectionWithoutReference(Bet); !ok || d != wallet.Debit {
		t.Error("BET debita")
	}
	if d, ok := DirectionWithoutReference(Win); !ok || d != wallet.Credit {
		t.Error("WIN credita")
	}
	if _, ok := DirectionWithoutReference(Loss); ok {
		t.Error("LOSS não movimenta")
	}
}
