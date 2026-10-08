package wallet

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/gabrielmilano/wager-wallet-service/internal/domain/money"
)

// ErrInvalidEntry indica um lançamento que viola as regras do extrato.
var ErrInvalidEntry = errors.New("wallet: lançamento inválido")

// Direction é o sentido de um lançamento.
type Direction string

const (
	Debit  Direction = "DEBIT"
	Credit Direction = "CREDIT"
)

// ParseDirection aceita apenas DEBIT ou CREDIT.
func ParseDirection(s string) (Direction, error) {
	switch d := Direction(s); d {
	case Debit, Credit:
		return d, nil
	default:
		return "", fmt.Errorf("%w: direção %q", ErrInvalidEntry, s)
	}
}

// Opposite devolve o sentido contrário (usado pelo ROLLBACK).
func (d Direction) Opposite() Direction {
	if d == Debit {
		return Credit
	}
	return Debit
}

// LedgerEntry é um lançamento imutável do extrato: só tem getters.
type LedgerEntry struct {
	id            uuid.UUID
	walletID      uuid.UUID
	transactionID uuid.UUID
	walletVersion int64
	direction     Direction
	amount        money.Money
	balanceBefore money.Money
	balanceAfter  money.Money
	createdAt     time.Time
}

// EntryParams reúne os campos de um lançamento.
type EntryParams struct {
	ID            uuid.UUID
	WalletID      uuid.UUID
	TransactionID uuid.UUID
	WalletVersion int64
	Direction     Direction
	Amount        money.Money
	BalanceBefore money.Money
	BalanceAfter  money.Money
	CreatedAt     time.Time
}

// NewLedgerEntry valida e cria um lançamento. Serve tanto para lançamentos
// novos quanto para reidratar os do banco: a regra
// balanceAfter = balanceBefore ± amount vale sempre.
func NewLedgerEntry(p EntryParams) (LedgerEntry, error) {
	invalid := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalidEntry, fmt.Sprintf(format, args...))
	}
	if p.ID == uuid.Nil || p.WalletID == uuid.Nil || p.TransactionID == uuid.Nil {
		return LedgerEntry{}, invalid("ids são obrigatórios")
	}
	if p.WalletVersion < 1 {
		return LedgerEntry{}, invalid("versão %d", p.WalletVersion)
	}
	if _, err := ParseDirection(string(p.Direction)); err != nil {
		return LedgerEntry{}, err
	}
	if !p.Amount.Valid() || !p.Amount.IsPositive() {
		return LedgerEntry{}, invalid("valor deve ser > 0 (recebido %s)", p.Amount)
	}
	if !p.BalanceBefore.Valid() || !p.BalanceAfter.Valid() ||
		p.BalanceBefore.IsNegative() || p.BalanceAfter.IsNegative() {
		return LedgerEntry{}, invalid("saldos devem ser >= 0")
	}

	var expected money.Money
	var err error
	if p.Direction == Credit {
		expected, err = p.BalanceBefore.Add(p.Amount)
	} else {
		expected, err = p.BalanceBefore.Sub(p.Amount)
	}
	if err != nil {
		return LedgerEntry{}, fmt.Errorf("%w: %v", ErrInvalidEntry, err)
	}
	if !expected.Equal(p.BalanceAfter) {
		return LedgerEntry{}, invalid("%s %s %s deveria dar %s, recebido %s",
			p.BalanceBefore, p.Direction, p.Amount, expected, p.BalanceAfter)
	}

	return LedgerEntry{
		id:            p.ID,
		walletID:      p.WalletID,
		transactionID: p.TransactionID,
		walletVersion: p.WalletVersion,
		direction:     p.Direction,
		amount:        p.Amount,
		balanceBefore: p.BalanceBefore,
		balanceAfter:  p.BalanceAfter,
		createdAt:     p.CreatedAt.UTC(),
	}, nil
}

func (e LedgerEntry) ID() uuid.UUID              { return e.id }
func (e LedgerEntry) WalletID() uuid.UUID        { return e.walletID }
func (e LedgerEntry) TransactionID() uuid.UUID   { return e.transactionID }
func (e LedgerEntry) WalletVersion() int64       { return e.walletVersion }
func (e LedgerEntry) Direction() Direction       { return e.direction }
func (e LedgerEntry) Amount() money.Money        { return e.amount }
func (e LedgerEntry) BalanceBefore() money.Money { return e.balanceBefore }
func (e LedgerEntry) BalanceAfter() money.Money  { return e.balanceAfter }
func (e LedgerEntry) CreatedAt() time.Time       { return e.createdAt }
