package wallet

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gabrielmilano/wager-wallet-service/internal/domain/money"
)

var now = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

func brl(t *testing.T, amount string) money.Money {
	t.Helper()
	m, err := money.Parse(amount, "BRL")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func open(t *testing.T, initial string) (*Wallet, *LedgerEntry) {
	t.Helper()
	w, e, err := Open(OpenParams{
		ID: uuid.New(), PlayerID: uuid.New(), Initial: brl(t, initial),
		OpeningTxID: uuid.New(), EntryID: uuid.New(), Now: now,
	})
	if err != nil {
		t.Fatalf("Open(%s): %v", initial, err)
	}
	return w, e
}

func TestOpenWithPositiveBalance(t *testing.T) {
	w, e := open(t, "100.00")
	if w.Version() != 1 || w.Balance().Amount() != "100.00" || w.Currency() != money.BRL {
		t.Errorf("carteira = versão %d saldo %s", w.Version(), w.Balance())
	}
	if e == nil {
		t.Fatal("abertura positiva deveria gerar lançamento")
	}
	if e.WalletVersion() != 1 || e.Direction() != Credit || e.BalanceBefore().Amount() != "0.00" ||
		e.BalanceAfter().Amount() != "100.00" || e.WalletID() != w.ID() {
		t.Errorf("lançamento de abertura incorreto: %+v", e)
	}
}

func TestOpenWithZeroBalanceHasNoEntry(t *testing.T) {
	w, e := open(t, "0.00")
	if e != nil || w.Version() != 1 || !w.Balance().IsZero() {
		t.Errorf("abertura zero: versão %d, lançamento %v", w.Version(), e)
	}
}

func TestOpenRejectsInvalidInput(t *testing.T) {
	neg := brl(t, "-1.00")
	tests := []struct {
		name string
		p    OpenParams
		want error
	}{
		{"saldo negativo", OpenParams{ID: uuid.New(), PlayerID: uuid.New(), Initial: neg, Now: now}, ErrInvalidAmount},
		{"money não inicializado", OpenParams{ID: uuid.New(), PlayerID: uuid.New(), Now: now}, ErrInvalidAmount},
		{"sem id", OpenParams{PlayerID: uuid.New(), Initial: brl(t, "1.00"), Now: now}, ErrInvalidWallet},
		{"sem jogador", OpenParams{ID: uuid.New(), Initial: brl(t, "1.00"), Now: now}, ErrInvalidWallet},
	}
	for _, tt := range tests {
		if _, _, err := Open(tt.p); !errors.Is(err, tt.want) {
			t.Errorf("%s: erro = %v, want %v", tt.name, err, tt.want)
		}
	}
}

func TestDebitAndCredit(t *testing.T) {
	w, _ := open(t, "100.00")
	tx := uuid.New()

	e, err := w.Debit(tx, uuid.New(), brl(t, "80.00"), now)
	if err != nil {
		t.Fatal(err)
	}
	if w.Balance().Amount() != "20.00" || w.Version() != 2 {
		t.Errorf("após débito: saldo %s versão %d", w.Balance(), w.Version())
	}
	if e.Direction() != Debit || e.BalanceBefore().Amount() != "100.00" || e.BalanceAfter().Amount() != "20.00" ||
		e.WalletVersion() != 2 || e.TransactionID() != tx {
		t.Errorf("lançamento de débito incorreto: %+v", e)
	}

	e, err = w.Credit(uuid.New(), uuid.New(), brl(t, "5.50"), now)
	if err != nil {
		t.Fatal(err)
	}
	if w.Balance().Amount() != "25.50" || w.Version() != 3 || e.WalletVersion() != 3 {
		t.Errorf("após crédito: saldo %s versão %d", w.Balance(), w.Version())
	}
}

func TestDebitInsufficientFundsLeavesWalletUnchanged(t *testing.T) {
	w, _ := open(t, "100.00")
	if _, err := w.Debit(uuid.New(), uuid.New(), brl(t, "80.00"), now); err != nil {
		t.Fatal(err)
	}
	_, err := w.Debit(uuid.New(), uuid.New(), brl(t, "80.00"), now)
	if !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("erro = %v, want ErrInsufficientFunds", err)
	}
	if w.Balance().Amount() != "20.00" || w.Version() != 2 {
		t.Errorf("carteira mudou após recusa: saldo %s versão %d", w.Balance(), w.Version())
	}
	// Debitar exatamente o saldo é permitido (fica zero).
	if _, err := w.Debit(uuid.New(), uuid.New(), brl(t, "20.00"), now); err != nil || !w.Balance().IsZero() {
		t.Errorf("débito do saldo inteiro: %v, saldo %s", err, w.Balance())
	}
}

func TestMoveRejectsInvalidAmounts(t *testing.T) {
	w, _ := open(t, "100.00")
	usd, _ := money.Parse("1.00", "USD")
	tests := []struct {
		name   string
		amount money.Money
		want   error
	}{
		{"zero", brl(t, "0.00"), ErrInvalidAmount},
		{"negativo", brl(t, "-1.00"), ErrInvalidAmount},
		{"não inicializado", money.Money{}, ErrInvalidAmount},
		{"outra moeda", usd, ErrCurrencyMismatch},
	}
	for _, tt := range tests {
		for _, d := range []Direction{Debit, Credit} {
			if _, err := w.Move(d, uuid.New(), uuid.New(), tt.amount, now); !errors.Is(err, tt.want) {
				t.Errorf("%s %s: erro = %v, want %v", d, tt.name, err, tt.want)
			}
		}
	}
	if w.Version() != 1 || w.Balance().Amount() != "100.00" {
		t.Errorf("carteira mudou: versão %d saldo %s", w.Version(), w.Balance())
	}
}

func TestCreditOverflow(t *testing.T) {
	max, _ := money.New(1<<63-1, money.BRL)
	w, _, err := Open(OpenParams{ID: uuid.New(), PlayerID: uuid.New(), Initial: max,
		OpeningTxID: uuid.New(), EntryID: uuid.New(), Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Credit(uuid.New(), uuid.New(), brl(t, "0.01"), now); !errors.Is(err, money.ErrOverflow) {
		t.Errorf("erro = %v, want ErrOverflow", err)
	}
}

func TestRehydrateValidatesWithoutMoving(t *testing.T) {
	w, err := Rehydrate(uuid.New(), uuid.New(), brl(t, "20.00"), 7, now, now)
	if err != nil || w.Version() != 7 || w.Balance().Amount() != "20.00" {
		t.Errorf("Rehydrate = %v, %v", w, err)
	}
	if _, err := Rehydrate(uuid.New(), uuid.New(), brl(t, "-1.00"), 1, now, now); !errors.Is(err, ErrInvalidWallet) {
		t.Errorf("saldo negativo: %v", err)
	}
	if _, err := Rehydrate(uuid.New(), uuid.New(), brl(t, "1.00"), 0, now, now); !errors.Is(err, ErrInvalidWallet) {
		t.Errorf("versão 0: %v", err)
	}
}

func TestLedgerEntryArithmetic(t *testing.T) {
	base := EntryParams{
		ID: uuid.New(), WalletID: uuid.New(), TransactionID: uuid.New(), WalletVersion: 2,
		Direction: Debit, Amount: brl(t, "80.00"), BalanceBefore: brl(t, "100.00"),
		BalanceAfter: brl(t, "20.00"), CreatedAt: now,
	}
	if _, err := NewLedgerEntry(base); err != nil {
		t.Fatalf("lançamento válido: %v", err)
	}

	tests := []struct {
		name   string
		change func(p *EntryParams)
	}{
		{"conta errada", func(p *EntryParams) { p.BalanceAfter = brl(t, "25.00") }},
		{"direção trocada", func(p *EntryParams) { p.Direction = Credit }},
		{"valor zero", func(p *EntryParams) { p.Amount = brl(t, "0.00") }},
		{"saldo depois negativo", func(p *EntryParams) {
			p.BalanceBefore, p.Amount, p.BalanceAfter = brl(t, "10.00"), brl(t, "20.00"), brl(t, "-10.00")
		}},
		{"versão zero", func(p *EntryParams) { p.WalletVersion = 0 }},
		{"direção desconhecida", func(p *EntryParams) { p.Direction = "SIDEWAYS" }},
		{"sem transação", func(p *EntryParams) { p.TransactionID = uuid.Nil }},
	}
	for _, tt := range tests {
		p := base
		tt.change(&p)
		if _, err := NewLedgerEntry(p); !errors.Is(err, ErrInvalidEntry) {
			t.Errorf("%s: erro = %v, want ErrInvalidEntry", tt.name, err)
		}
	}
}
