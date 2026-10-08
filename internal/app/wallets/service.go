package wallets

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/gabrielmilano/wager-wallet-service/internal/app/apperr"
	"github.com/gabrielmilano/wager-wallet-service/internal/app/port"
	"github.com/gabrielmilano/wager-wallet-service/internal/app/store"
	"github.com/gabrielmilano/wager-wallet-service/internal/domain/event"
	"github.com/gabrielmilano/wager-wallet-service/internal/domain/money"
	"github.com/gabrielmilano/wager-wallet-service/internal/domain/wager"
	"github.com/gabrielmilano/wager-wallet-service/internal/domain/wallet"
)

// Paginação do extrato.
const (
	DefaultPageSize = 50
	MaxPageSize     = 200
)

// Service abre carteiras e atende às consultas de carteira e extrato.
type Service struct {
	tx      store.TxRunner
	clock   port.Clock
	ids     port.IDGenerator
	metrics port.Metrics
}

func NewService(tx store.TxRunner, clock port.Clock, ids port.IDGenerator) *Service {
	return &Service{tx: tx, clock: clock, ids: ids, metrics: port.NopMetrics{}}
}

// WithMetrics liga as métricas (opcional).
func (s *Service) WithMetrics(m port.Metrics) *Service {
	s.metrics = m
	return s
}

// Reconciliation compara o saldo gravado com o reconstruído pelo extrato.
type Reconciliation struct {
	WalletID          uuid.UUID
	StoredBalance     money.Money
	CalculatedBalance money.Money
	Difference        money.Money // gravado - reconstruído
	Consistent        bool
	CheckedEntries    int
}

// Reconcile reconstrói o saldo a partir do extrato (incluindo a abertura) e
// compara com o gravado, numa foto consistente do banco (REPEATABLE READ,
// somente leitura). Não altera nada.
func (s *Service) Reconcile(ctx context.Context, walletID uuid.UUID) (Reconciliation, error) {
	var rec Reconciliation
	err := s.tx.ReadSnapshot(ctx, func(ctx context.Context, r store.Repos) error {
		w, err := r.Wallets.Get(ctx, walletID)
		if err != nil {
			return err
		}
		credits, debits, entries, err := r.Ledger.Totals(ctx, walletID)
		if err != nil {
			return err
		}
		c := w.Currency()
		creditsM, err := money.New(credits, c)
		if err != nil {
			return err
		}
		debitsM, err := money.New(debits, c)
		if err != nil {
			return err
		}
		calculated, err := creditsM.Sub(debitsM)
		if err != nil {
			return err
		}
		diff, err := w.Balance().Sub(calculated)
		if err != nil {
			return err
		}
		rec = Reconciliation{
			WalletID: walletID, StoredBalance: w.Balance(), CalculatedBalance: calculated,
			Difference: diff, Consistent: diff.IsZero(), CheckedEntries: entries,
		}
		return nil
	})
	if err != nil {
		return Reconciliation{}, classify(err)
	}
	s.metrics.Reconciliation(rec.Consistent)
	return rec, nil
}

// OpenInput é a abertura como chega da API (o Money já foi decodificado).
type OpenInput struct {
	PlayerID       string
	InitialBalance money.Money
	CorrelationID  string
}

// Open abre a carteira. Com saldo inicial positivo, grava no mesmo commit a
// OPENING (PROCESSED), o lançamento de crédito (versão 1) e os eventos
// WagerTransactionProcessed e WalletBalanceChanged. Com saldo zero, só a
// carteira. Carteira repetida para o jogador e moeda: WALLET_ALREADY_EXISTS.
func (s *Service) Open(ctx context.Context, in OpenInput) (*wallet.Wallet, error) {
	playerID, err := uuid.Parse(in.PlayerID)
	switch {
	case len(in.PlayerID) != 36 || err != nil || playerID == uuid.Nil:
		return nil, apperr.ValidationError("playerId deve ser um UUID válido", nil)
	case !in.InitialBalance.Valid():
		return nil, apperr.ValidationError("initialBalance é obrigatório", nil)
	case in.InitialBalance.IsNegative():
		return nil, apperr.ValidationError("initialBalance.amount não pode ser negativo", nil)
	}

	now := s.clock.Now()
	w, entry, err := wallet.Open(wallet.OpenParams{
		ID: s.ids.NewID(), PlayerID: playerID, Initial: in.InitialBalance,
		OpeningTxID: s.ids.NewID(), EntryID: s.ids.NewID(), Now: now,
	})
	if err != nil {
		return nil, apperr.ValidationError(err.Error(), err)
	}

	err = s.tx.WithinTx(ctx, func(ctx context.Context, r store.Repos) error {
		if err := r.Wallets.Insert(ctx, w); err != nil {
			if errors.Is(err, store.ErrConflict) {
				return apperr.New(apperr.Conflict, apperr.CodeWalletAlreadyExists,
					"o jogador já tem carteira nessa moeda", nil)
			}
			return err
		}
		if entry == nil {
			return nil
		}

		opening, err := wager.NewOpening(entry.TransactionID(), w.ID(), w.PlayerID(), in.InitialBalance, now)
		if err != nil {
			return err
		}
		if err := r.Transactions.InsertOpening(ctx, opening); err != nil {
			return err
		}
		if err := r.Ledger.Insert(ctx, *entry); err != nil {
			return err
		}

		correlation := in.CorrelationID
		if correlation == "" {
			correlation = opening.ID().String()
		}
		meta := func() event.Meta {
			return event.Meta{EventID: s.ids.NewID(), CorrelationID: correlation, OccurredAt: now}
		}
		processed, err := event.NewWagerTransactionProcessed(meta(), opening)
		if err != nil {
			return err
		}
		changed, err := event.NewWalletBalanceChanged(meta(), *entry)
		if err != nil {
			return err
		}
		return r.Outbox.Insert(ctx, processed, changed)
	})
	if err != nil {
		return nil, classify(err)
	}
	return w, nil
}

// Get lê a carteira.
func (s *Service) Get(ctx context.Context, id uuid.UUID) (*wallet.Wallet, error) {
	var w *wallet.Wallet
	err := s.tx.WithinTx(ctx, func(ctx context.Context, r store.Repos) error {
		var err error
		w, err = r.Wallets.Get(ctx, id)
		return err
	})
	return w, classify(err)
}

// LedgerPage é uma página do extrato. NextCursor vazio: não há mais páginas.
type LedgerPage struct {
	Entries    []wallet.LedgerEntry
	NextCursor string
}

// Ledger devolve uma página do extrato em ordem crescente de versão. O
// cursor é opaco para o cliente (base64 da última versão lida).
func (s *Service) Ledger(ctx context.Context, walletID uuid.UUID, cursor string, limit int) (LedgerPage, error) {
	after, err := decodeCursor(cursor)
	if err != nil {
		return LedgerPage{}, err
	}
	switch {
	case limit == 0:
		limit = DefaultPageSize
	case limit < 0 || limit > MaxPageSize:
		return LedgerPage{}, apperr.ValidationError(fmt.Sprintf("limit deve estar entre 1 e %d", MaxPageSize), nil)
	}

	var page LedgerPage
	err = s.tx.WithinTx(ctx, func(ctx context.Context, r store.Repos) error {
		if _, err := r.Wallets.Get(ctx, walletID); err != nil {
			return err
		}
		// Um a mais, para saber se existe próxima página.
		entries, err := r.Ledger.List(ctx, walletID, after, limit+1)
		if err != nil {
			return err
		}
		if len(entries) > limit {
			entries = entries[:limit]
			page.NextCursor = encodeCursor(entries[len(entries)-1].WalletVersion())
		}
		page.Entries = entries
		return nil
	})
	return page, classify(err)
}

func encodeCursor(version int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte("v1:" + strconv.FormatInt(version, 10)))
}

func decodeCursor(cursor string) (int64, error) {
	if cursor == "" {
		return 0, nil
	}
	invalid := apperr.ValidationError("cursor inválido", nil)
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, invalid
	}
	digits, ok := strings.CutPrefix(string(raw), "v1:")
	if !ok {
		return 0, invalid
	}
	version, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || version < 1 {
		return 0, invalid
	}
	return version, nil
}

func classify(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrNotFound):
		return apperr.New(apperr.NotFound, apperr.CodeWalletNotFound, "carteira não encontrada", nil)
	case errors.Is(err, store.ErrUnavailable):
		return apperr.UnavailableError(err)
	}
	return err
}
