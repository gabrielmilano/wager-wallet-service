package wagering

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/gabrielmilano/wager-wallet-service/internal/app/apperr"
	"github.com/gabrielmilano/wager-wallet-service/internal/app/port"
	"github.com/gabrielmilano/wager-wallet-service/internal/app/store"
	"github.com/gabrielmilano/wager-wallet-service/internal/domain/event"
	"github.com/gabrielmilano/wager-wallet-service/internal/domain/wager"
	"github.com/gabrielmilano/wager-wallet-service/internal/domain/wallet"
)

// Política de espera por referência: primeira retentativa em 1 s, dobrando
// até no máximo 5 min, e prazo total de 1 h (modelagem aprovada).
const (
	FirstRetryDelay = time.Second
	MaxRetryDelay   = 5 * time.Minute
	ReferenceTTL    = time.Hour
)

// Service processa operações de provedores. HTTP e SQS usam o mesmo núcleo
// (apply); as entradas diferem só no que envolvem ao redor dele (ADR 0006).
type Service struct {
	tx    store.TxRunner
	clock port.Clock
	ids   port.IDGenerator
}

func NewService(tx store.TxRunner, clock port.Clock, ids port.IDGenerator) *Service {
	return &Service{tx: tx, clock: clock, ids: ids}
}

// Result é o resultado de uma operação. Para replays, é o resultado
// persistido no processamento original.
type Result struct {
	Transaction *wager.Transaction
	Replay      bool
}

// InboundMessage identifica uma mensagem SQS na inbox.
type InboundMessage struct {
	ConsumerName string
	MessageID    string
	PayloadHash  []byte
}

// Process processa uma operação recebida por HTTP.
func (s *Service) Process(ctx context.Context, cmd Command, meta Metadata) (Result, error) {
	if err := ValidateMetadata(meta); err != nil {
		return Result{}, err
	}
	var res Result
	err := s.tx.WithinTx(ctx, func(ctx context.Context, r store.Repos) error {
		var err error
		res, err = s.apply(ctx, r, cmd, meta)
		return err
	})
	return res, classify(err)
}

// ProcessFromMessage processa uma operação recebida por SQS. A inbox é
// gravada e concluída na mesma transação SQL das alterações de domínio, do
// extrato e da outbox. Uma mensagem já registrada devolve o resultado
// persistido; o mesmo messageId com conteúdo diferente é MESSAGE_CONFLICT.
func (s *Service) ProcessFromMessage(ctx context.Context, cmd Command, meta Metadata, msg InboundMessage) (Result, error) {
	if err := ValidateMetadata(meta); err != nil {
		return Result{}, err
	}
	if msg.ConsumerName == "" || msg.MessageID == "" || len(msg.PayloadHash) == 0 {
		return Result{}, apperr.ValidationError("mensagem sem consumidor, messageId ou hash", nil)
	}

	var res Result
	err := s.tx.WithinTx(ctx, func(ctx context.Context, r store.Repos) error {
		now := s.clock.Now()
		inserted, err := r.Inbox.Insert(ctx, store.InboxMessage{
			ConsumerName: msg.ConsumerName, MessageID: msg.MessageID,
			PayloadHash: msg.PayloadHash, ReceivedAt: now,
		})
		if err != nil {
			return err
		}
		if !inserted {
			res, err = s.inboxReplay(ctx, r, msg)
			return err
		}

		res, err = s.apply(ctx, r, cmd, meta)
		if err != nil {
			return err
		}
		return r.Inbox.MarkProcessed(ctx, msg.ConsumerName, msg.MessageID, res.Transaction.ID(), now)
	})
	return res, classify(err)
}

// inboxReplay trata a reentrega de uma mensagem já concluída.
func (s *Service) inboxReplay(ctx context.Context, r store.Repos, msg InboundMessage) (Result, error) {
	existing, err := r.Inbox.Get(ctx, msg.ConsumerName, msg.MessageID)
	if err != nil {
		return Result{}, err
	}
	if !bytes.Equal(existing.PayloadHash, msg.PayloadHash) {
		return Result{}, apperr.New(apperr.Conflict, apperr.CodeMessageConflict,
			"mesmo messageId com conteúdo diferente", nil)
	}
	if existing.TransactionID == nil {
		return Result{}, fmt.Errorf("inbox %s/%s concluída sem operação", msg.ConsumerName, msg.MessageID)
	}
	tx, err := r.Transactions.GetByID(ctx, *existing.TransactionID)
	if err != nil {
		return Result{}, err
	}
	return Result{Transaction: tx, Replay: true}, nil
}

// GetTransaction lê uma operação pelo id interno.
func (s *Service) GetTransaction(ctx context.Context, id uuid.UUID) (*wager.Transaction, error) {
	var tx *wager.Transaction
	err := s.tx.WithinTx(ctx, func(ctx context.Context, r store.Repos) error {
		var err error
		tx, err = r.Transactions.GetByID(ctx, id)
		return err
	})
	return tx, classify(notFoundAs(err, apperr.CodeTransactionNotFound, "operação não encontrada"))
}

// GetByExternalID lê uma operação por (provedor, id externo).
func (s *Service) GetByExternalID(ctx context.Context, providerID, externalID string) (*wager.Transaction, error) {
	var tx *wager.Transaction
	err := s.tx.WithinTx(ctx, func(ctx context.Context, r store.Repos) error {
		var err error
		tx, err = r.Transactions.FindByExternalID(ctx, providerID, externalID)
		return err
	})
	return tx, classify(notFoundAs(err, apperr.CodeTransactionNotFound, "operação não encontrada"))
}

// --- núcleo ------------------------------------------------------------------

// apply executa uma operação dentro da transação já aberta. Ordem dos locks:
// (inbox →) operação → carteira, igual em todos os caminhos (sem deadlock).
func (s *Service) apply(ctx context.Context, r store.Repos, cmd Command, meta Metadata) (Result, error) {
	hash := cmd.Hash()

	// 1. Idempotência: replay ou conflito, sem tocar na carteira.
	if res, found, err := lookup(ctx, r, cmd, meta.IdempotencyKey, hash); err != nil || found {
		return res, err
	}

	// 2. A carteira existe e é do jogador e moeda informados (4xx, nada gravado).
	w, err := r.Wallets.Get(ctx, cmd.WalletID)
	if errors.Is(err, store.ErrNotFound) {
		return Result{}, apperr.New(apperr.NotFound, apperr.CodeWalletNotFound, "carteira não encontrada", nil)
	}
	if err != nil {
		return Result{}, err
	}
	if w.PlayerID() != cmd.PlayerID || w.Currency() != cmd.Money.Currency() {
		return Result{}, apperr.New(apperr.Validation, apperr.CodeWalletMismatch,
			"jogador ou moeda diferentes dos da carteira", nil)
	}

	// 3. Protocolo em PENDING. Se outra requisição com a mesma operação
	// chegou antes (corrida), o INSERT não grava e a busca devolve o resultado
	// dela: o INSERT espera a transação concorrente terminar.
	now := s.clock.Now()
	tx, err := wager.NewExternal(wager.ExternalParams{
		ID: s.ids.NewID(), WalletID: cmd.WalletID, PlayerID: cmd.PlayerID,
		ProviderID: cmd.ProviderID, ExternalID: cmd.ExternalTransactionID,
		IdempotencyKey: meta.IdempotencyKey, PayloadHash: hash,
		RoundID: cmd.RoundID, GameID: cmd.GameID, Kind: cmd.Kind, Money: cmd.Money,
		ReferenceExternalID: cmd.ReferenceExternalTransactionID, Now: now,
	})
	if err != nil {
		return Result{}, apperr.ValidationError(err.Error(), err)
	}
	inserted, err := r.Transactions.InsertPending(ctx, tx)
	if err != nil {
		return Result{}, err
	}
	if !inserted {
		res, found, err := lookup(ctx, r, cmd, meta.IdempotencyKey, hash)
		if err == nil && !found {
			err = fmt.Errorf("operação %s/%s conflitou, mas não foi encontrada", cmd.ProviderID, cmd.ExternalTransactionID)
		}
		return res, err
	}

	// 4. Fila por carteira: quem chega depois espera (lock_timeout → 503).
	w, err = r.Wallets.GetForUpdate(ctx, cmd.WalletID)
	if err != nil {
		return Result{}, err
	}

	// 5. Decisão de negócio (domínio puro) e 6. gravação.
	out, err := s.decide(ctx, r, tx, w, now)
	if err != nil {
		return Result{}, err
	}
	if err := s.persist(ctx, r, tx, w, out, meta, now); err != nil {
		return Result{}, err
	}
	return Result{Transaction: tx}, nil
}

// lookup procura a operação pela chave e pelo id externo do provedor.
//   - mesma chave e mesmo conteúdo: replay do resultado persistido;
//   - mesma chave, conteúdo diferente: IDEMPOTENCY_CONFLICT;
//   - mesmo id externo com outra chave: IDEMPOTENCY_CONFLICT (decisão Q1).
//
// As duas buscas são comandos separados e, em READ COMMITTED, cada uma vê um
// snapshot novo: uma requisição concorrente pode confirmar entre elas. Por
// isso a decisão compara a chave do registro encontrado, em vez de supor que
// achar pelo id externo significa "outra chave".
func lookup(ctx context.Context, r store.Repos, cmd Command, key string, hash []byte) (Result, bool, error) {
	existing, err := r.Transactions.FindByIdempotencyKey(ctx, cmd.ProviderID, key)
	if errors.Is(err, store.ErrNotFound) {
		existing, err = r.Transactions.FindByExternalID(ctx, cmd.ProviderID, cmd.ExternalTransactionID)
	}
	switch {
	case errors.Is(err, store.ErrNotFound):
		return Result{}, false, nil
	case err != nil:
		return Result{}, false, err
	case existing.IdempotencyKey() != key:
		return Result{}, true, apperr.New(apperr.Conflict, apperr.CodeIdempotencyConflict,
			"operação já registrada com outra Idempotency-Key", nil)
	case !existing.SamePayload(hash):
		return Result{}, true, apperr.New(apperr.Conflict, apperr.CodeIdempotencyConflict,
			"Idempotency-Key já usada com outro conteúdo", nil)
	}
	return Result{Transaction: existing, Replay: true}, true, nil
}

// outcome é o que a decisão produziu: o lançamento (se houve movimentação).
type outcome struct {
	entry *wallet.LedgerEntry
}

// decide aplica as regras dos cinco tipos externos. Muda tx (e w, se houver
// movimentação) em memória; nada é gravado aqui.
func (s *Service) decide(ctx context.Context, r store.Repos, tx *wager.Transaction, w *wallet.Wallet, now time.Time) (outcome, error) {
	kind := tx.Kind()

	if kind == wager.Loss {
		return outcome{}, tx.Process(w.Balance(), nil, now)
	}
	if tx.ReferenceExternalID() == "" {
		direction, _ := wager.DirectionWithoutReference(kind)
		return s.move(tx, w, direction, nil, now)
	}

	original, err := r.Transactions.FindByExternalID(ctx, tx.ProviderID(), tx.ReferenceExternalID())
	if errors.Is(err, store.ErrNotFound) {
		original, err = nil, nil
	}
	if err != nil {
		return outcome{}, err
	}
	reversed := false
	if original != nil && (kind == wager.Refund || kind == wager.Rollback) {
		if reversed, err = r.Transactions.HasProcessedReversal(ctx, original.ID()); err != nil {
			return outcome{}, err
		}
	}

	res := wager.ResolveReference(tx, original, reversed)
	switch res.Outcome {
	case wager.Wait:
		return outcome{}, tx.WaitForReference(now.Add(FirstRetryDelay), now.Add(ReferenceTTL), now)
	case wager.Reject:
		ref := res.ReferenceID
		return outcome{}, tx.Reject(res.Code, w.Balance(), &ref, now)
	default:
		ref := res.ReferenceID
		return s.move(tx, w, res.Direction, &ref, now)
	}
}

// move aplica o débito ou crédito. Saldo insuficiente é rejeição definitiva
// (com código próprio para reversões), não erro.
func (s *Service) move(tx *wager.Transaction, w *wallet.Wallet, d wallet.Direction, ref *uuid.UUID, now time.Time) (outcome, error) {
	entry, err := w.Move(d, tx.ID(), s.ids.NewID(), tx.Money(), now)
	if errors.Is(err, wallet.ErrInsufficientFunds) {
		return outcome{}, tx.Reject(wager.InsufficientFundsCode(tx.Kind()), w.Balance(), ref, now)
	}
	if err != nil {
		return outcome{}, err
	}
	if err := tx.Process(w.Balance(), ref, now); err != nil {
		return outcome{}, err
	}
	return outcome{entry: &entry}, nil
}

// persist grava a decisão: operação (um único UPDATE até o estado final),
// saldo, extrato e eventos. A operação vem antes do extrato porque o trigger
// do extrato confere a direção usando a referência resolvida (ADR 0011).
func (s *Service) persist(ctx context.Context, r store.Repos, tx *wager.Transaction, w *wallet.Wallet, out outcome, meta Metadata, now time.Time) error {
	if err := r.Transactions.Update(ctx, tx); err != nil {
		return err
	}
	if out.entry != nil {
		if err := r.Wallets.UpdateBalance(ctx, w); err != nil {
			return err
		}
		if err := r.Ledger.Insert(ctx, *out.entry); err != nil {
			return err
		}
	}
	events, err := s.events(tx, out, meta, now)
	if err != nil {
		return err
	}
	return r.Outbox.Insert(ctx, events...)
}

// events monta os eventos da decisão: Processed (+ BalanceChanged se houve
// lançamento), Rejected ou PendingReference.
func (s *Service) events(tx *wager.Transaction, out outcome, meta Metadata, now time.Time) ([]event.Envelope, error) {
	m := func() event.Meta {
		correlation := meta.CorrelationID
		if correlation == "" {
			correlation = tx.ID().String()
		}
		return event.Meta{EventID: s.ids.NewID(), CorrelationID: correlation, CausationID: meta.CausationID, OccurredAt: now}
	}

	var events []event.Envelope
	var ev event.Envelope
	var err error
	switch tx.Status() {
	case wager.Processed:
		if ev, err = event.NewWagerTransactionProcessed(m(), tx); err != nil {
			return nil, err
		}
		events = append(events, ev)
		if out.entry != nil {
			if ev, err = event.NewWalletBalanceChanged(m(), *out.entry); err != nil {
				return nil, err
			}
			events = append(events, ev)
		}
	case wager.Rejected:
		if ev, err = event.NewWagerTransactionRejected(m(), tx); err != nil {
			return nil, err
		}
		events = append(events, ev)
	case wager.PendingReference:
		if ev, err = event.NewWagerTransactionPendingReference(m(), tx); err != nil {
			return nil, err
		}
		events = append(events, ev)
	default:
		return nil, fmt.Errorf("operação %s em %s não gera evento", tx.ID(), tx.Status())
	}
	return events, nil
}

// --- erros -------------------------------------------------------------------

// classify converte erros de infraestrutura em apperr.Unavailable; os demais
// passam adiante (os já classificados e os inesperados, que viram 500).
func classify(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := apperr.As(err); ok {
		return err
	}
	if errors.Is(err, store.ErrUnavailable) {
		return apperr.UnavailableError(err)
	}
	return err
}

func notFoundAs(err error, code, message string) error {
	if errors.Is(err, store.ErrNotFound) {
		return apperr.New(apperr.NotFound, code, message, nil)
	}
	return err
}
