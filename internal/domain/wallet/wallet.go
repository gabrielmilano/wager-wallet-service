package wallet

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/gabrielmilano/wager-wallet-service/internal/domain/money"
)

// Erros do pacote, classificáveis com errors.Is.
var (
	ErrInvalidWallet     = errors.New("wallet: carteira inválida")
	ErrInvalidAmount     = errors.New("wallet: valor de movimentação inválido")
	ErrCurrencyMismatch  = errors.New("wallet: moeda diferente da carteira")
	ErrInsufficientFunds = errors.New("wallet: saldo insuficiente")
)

// Wallet é a raiz do agregado financeiro. O saldo só muda por Debit e Credit,
// que devolvem o lançamento correspondente; quem persiste os dois na mesma
// transação SQL é a camada de aplicação.
type Wallet struct {
	id        uuid.UUID
	playerID  uuid.UUID
	balance   money.Money
	version   int64
	createdAt time.Time
	updatedAt time.Time
}

// OpenParams reúne os dados da abertura. OpeningTxID e EntryID só são usados
// quando o saldo inicial é positivo.
type OpenParams struct {
	ID          uuid.UUID
	PlayerID    uuid.UUID
	Initial     money.Money
	OpeningTxID uuid.UUID
	EntryID     uuid.UUID
	Now         time.Time
}

// Open cria uma carteira na versão 1. Com saldo inicial positivo, devolve
// também o lançamento de crédito da abertura (versão 1); com saldo zero, não
// há lançamento.
func Open(p OpenParams) (*Wallet, *LedgerEntry, error) {
	if p.ID == uuid.Nil || p.PlayerID == uuid.Nil || p.Now.IsZero() {
		return nil, nil, fmt.Errorf("%w: id, jogador e instante são obrigatórios", ErrInvalidWallet)
	}
	if !p.Initial.Valid() || p.Initial.IsNegative() {
		return nil, nil, fmt.Errorf("%w: saldo inicial deve ser >= 0", ErrInvalidAmount)
	}
	now := p.Now.UTC()
	w := &Wallet{
		id:        p.ID,
		playerID:  p.PlayerID,
		balance:   p.Initial,
		version:   1,
		createdAt: now,
		updatedAt: now,
	}
	if p.Initial.IsZero() {
		return w, nil, nil
	}

	zero, _ := money.Zero(p.Initial.Currency())
	entry, err := NewLedgerEntry(EntryParams{
		ID:            p.EntryID,
		WalletID:      w.id,
		TransactionID: p.OpeningTxID,
		WalletVersion: 1,
		Direction:     Credit,
		Amount:        p.Initial,
		BalanceBefore: zero,
		BalanceAfter:  p.Initial,
		CreatedAt:     now,
	})
	if err != nil {
		return nil, nil, err
	}
	return w, &entry, nil
}

// Rehydrate reconstrói uma carteira já persistida, sem reaplicar
// movimentações; só confere as invariantes.
func Rehydrate(id, playerID uuid.UUID, balance money.Money, version int64, createdAt, updatedAt time.Time) (*Wallet, error) {
	if id == uuid.Nil || playerID == uuid.Nil {
		return nil, fmt.Errorf("%w: id e jogador são obrigatórios", ErrInvalidWallet)
	}
	if !balance.Valid() || balance.IsNegative() {
		return nil, fmt.Errorf("%w: saldo %s", ErrInvalidWallet, balance)
	}
	if version < 1 {
		return nil, fmt.Errorf("%w: versão %d", ErrInvalidWallet, version)
	}
	return &Wallet{
		id: id, playerID: playerID, balance: balance, version: version,
		createdAt: createdAt.UTC(), updatedAt: updatedAt.UTC(),
	}, nil
}

func (w *Wallet) ID() uuid.UUID            { return w.id }
func (w *Wallet) PlayerID() uuid.UUID      { return w.playerID }
func (w *Wallet) Currency() money.Currency { return w.balance.Currency() }
func (w *Wallet) Balance() money.Money     { return w.balance }
func (w *Wallet) Version() int64           { return w.version }
func (w *Wallet) CreatedAt() time.Time     { return w.createdAt }
func (w *Wallet) UpdatedAt() time.Time     { return w.updatedAt }

// Debit tira amount do saldo. Recusa com ErrInsufficientFunds se o saldo
// ficaria negativo; nesse caso a carteira não muda.
func (w *Wallet) Debit(txID, entryID uuid.UUID, amount money.Money, now time.Time) (LedgerEntry, error) {
	return w.move(Debit, txID, entryID, amount, now)
}

// Credit soma amount ao saldo.
func (w *Wallet) Credit(txID, entryID uuid.UUID, amount money.Money, now time.Time) (LedgerEntry, error) {
	return w.move(Credit, txID, entryID, amount, now)
}

// Move aplica a direção informada (atalho para quem decide a direção em
// tempo de execução, como um ROLLBACK).
func (w *Wallet) Move(d Direction, txID, entryID uuid.UUID, amount money.Money, now time.Time) (LedgerEntry, error) {
	return w.move(d, txID, entryID, amount, now)
}

func (w *Wallet) move(d Direction, txID, entryID uuid.UUID, amount money.Money, now time.Time) (LedgerEntry, error) {
	if !amount.Valid() || !amount.IsPositive() {
		return LedgerEntry{}, fmt.Errorf("%w: movimentação exige valor > 0 (recebido %s)", ErrInvalidAmount, amount)
	}
	if amount.Currency() != w.Currency() {
		return LedgerEntry{}, fmt.Errorf("%w: carteira em %s, valor em %s", ErrCurrencyMismatch, w.Currency(), amount.Currency())
	}

	var after money.Money
	var err error
	switch d {
	case Debit:
		after, err = w.balance.Sub(amount)
		if err == nil && after.IsNegative() {
			return LedgerEntry{}, fmt.Errorf("%w: saldo %s, débito %s", ErrInsufficientFunds, w.balance, amount)
		}
	case Credit:
		after, err = w.balance.Add(amount)
	default:
		return LedgerEntry{}, fmt.Errorf("%w: direção %q", ErrInvalidAmount, d)
	}
	if err != nil {
		return LedgerEntry{}, err
	}

	entry, err := NewLedgerEntry(EntryParams{
		ID:            entryID,
		WalletID:      w.id,
		TransactionID: txID,
		WalletVersion: w.version + 1,
		Direction:     d,
		Amount:        amount,
		BalanceBefore: w.balance,
		BalanceAfter:  after,
		CreatedAt:     now,
	})
	if err != nil {
		return LedgerEntry{}, err
	}

	// Só muda o estado depois de tudo validado.
	w.balance = after
	w.version++
	w.updatedAt = now.UTC()
	return entry, nil
}
