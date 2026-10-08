package wager

import (
	"bytes"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/gabrielmilano/wager-wallet-service/internal/domain/money"
)

// Transaction é o protocolo de uma operação. Estado encapsulado: muda só
// pelos métodos de transição, que validam a máquina de estados (a mesma que o
// banco impõe; ADR 0011).
type Transaction struct {
	id       uuid.UUID
	origin   Origin
	kind     Kind
	status   Status
	walletID uuid.UUID
	playerID uuid.UUID
	money    money.Money

	providerID          string
	externalID          string
	idempotencyKey      string
	payloadHash         []byte
	roundID             string
	gameID              string
	referenceExternalID string
	referenceTxID       *uuid.UUID

	failureCode   FailureCode
	resultBalance *money.Money

	attempts      int
	nextAttemptAt *time.Time
	expiresAt     *time.Time

	createdAt   time.Time
	updatedAt   time.Time
	completedAt *time.Time
}

// ExternalParams são os dados de uma operação recebida de um provedor.
type ExternalParams struct {
	ID                  uuid.UUID
	WalletID            uuid.UUID
	PlayerID            uuid.UUID
	ProviderID          string
	ExternalID          string
	IdempotencyKey      string
	PayloadHash         []byte
	RoundID             string
	GameID              string
	Kind                Kind
	Money               money.Money
	ReferenceExternalID string
	Now                 time.Time
}

// NewExternal cria uma operação de provedor em PENDING, validando a política
// de valores por tipo: LOSS exige zero; BET, WIN, REFUND e ROLLBACK exigem
// valor > 0; nenhum aceita negativo.
func NewExternal(p ExternalParams) (*Transaction, error) {
	invalid := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalidTransaction, fmt.Sprintf(format, args...))
	}
	if _, err := ParseExternalKind(string(p.Kind)); err != nil {
		return nil, err
	}
	if p.ID == uuid.Nil || p.WalletID == uuid.Nil || p.PlayerID == uuid.Nil || p.Now.IsZero() {
		return nil, invalid("id, carteira, jogador e instante são obrigatórios")
	}
	if p.ProviderID == "" || p.ExternalID == "" || p.IdempotencyKey == "" ||
		p.RoundID == "" || p.GameID == "" || len(p.PayloadHash) == 0 {
		return nil, invalid("provedor, id externo, chave, rodada, jogo e hash são obrigatórios")
	}
	if err := ValidateAmount(p.Kind, p.Money); err != nil {
		return nil, err
	}
	switch {
	case p.Kind.RequiresReference() && p.ReferenceExternalID == "":
		return nil, invalid("%s exige referenceExternalTransactionId", p.Kind)
	case !p.Kind.AcceptsReference() && p.ReferenceExternalID != "":
		return nil, invalid("%s não aceita referência", p.Kind)
	}

	now := p.Now.UTC()
	return &Transaction{
		id: p.ID, origin: External, kind: p.Kind, status: Pending,
		walletID: p.WalletID, playerID: p.PlayerID, money: p.Money,
		providerID: p.ProviderID, externalID: p.ExternalID, idempotencyKey: p.IdempotencyKey,
		payloadHash: bytes.Clone(p.PayloadHash), roundID: p.RoundID, gameID: p.GameID,
		referenceExternalID: p.ReferenceExternalID,
		createdAt:           now, updatedAt: now,
	}, nil
}

// ValidateAmount aplica a política de valores por tipo.
func ValidateAmount(k Kind, m money.Money) error {
	switch {
	case !m.Valid():
		return fmt.Errorf("%w: money ausente", ErrInvalidTransaction)
	case k == Loss && !m.IsZero():
		return fmt.Errorf("%w: LOSS exige amount 0.00 (recebido %s)", ErrInvalidTransaction, m.Amount())
	case k != Loss && !m.IsPositive():
		return fmt.Errorf("%w: %s exige amount > 0 (recebido %s)", ErrInvalidTransaction, k, m.Amount())
	}
	return nil
}

// NewOpening cria a operação interna de abertura, já PROCESSED. Não tem
// provedor, chave, hash, rodada, jogo nem referência.
func NewOpening(id, walletID, playerID uuid.UUID, amount money.Money, now time.Time) (*Transaction, error) {
	if id == uuid.Nil || walletID == uuid.Nil || playerID == uuid.Nil || now.IsZero() {
		return nil, fmt.Errorf("%w: id, carteira, jogador e instante são obrigatórios", ErrInvalidTransaction)
	}
	if !amount.Valid() || !amount.IsPositive() {
		return nil, fmt.Errorf("%w: OPENING só existe com saldo inicial > 0", ErrInvalidTransaction)
	}
	now = now.UTC()
	result := amount
	return &Transaction{
		id: id, origin: Internal, kind: Opening, status: Processed,
		walletID: walletID, playerID: playerID, money: amount,
		resultBalance: &result,
		createdAt:     now, updatedAt: now, completedAt: &now,
	}, nil
}

// --- transições --------------------------------------------------------------

func (t *Transaction) transitionError(to Status) error {
	return fmt.Errorf("%w: %s -> %s (operação %s)", ErrInvalidTransition, t.status, to, t.id)
}

// Process conclui a operação com sucesso. result é o saldo observado, que o
// replay devolve. referenceTxID é a referência resolvida (nil se não houver).
// Permitido a partir de PENDING e PENDING_REFERENCE.
func (t *Transaction) Process(result money.Money, referenceTxID *uuid.UUID, now time.Time) error {
	if t.status != Pending && t.status != PendingReference {
		return t.transitionError(Processed)
	}
	if !result.Valid() || result.IsNegative() {
		return fmt.Errorf("%w: saldo resultante inválido %s", ErrInvalidTransaction, result)
	}
	t.finish(Processed, "", result, referenceTxID, now)
	return nil
}

// Reject recusa a operação por regra de negócio (estado terminal). result é
// o saldo observado no momento da recusa.
func (t *Transaction) Reject(code FailureCode, result money.Money, referenceTxID *uuid.UUID, now time.Time) error {
	if t.status != Pending && t.status != PendingReference {
		return t.transitionError(Rejected)
	}
	if code == "" || !result.Valid() || result.IsNegative() {
		return fmt.Errorf("%w: rejeição exige código e saldo válido", ErrInvalidTransaction)
	}
	t.finish(Rejected, code, result, referenceTxID, now)
	return nil
}

// Fail registra falha permanente de infraestrutura. Só a partir de
// PENDING_REFERENCE (decisão 3): no fluxo síncrono, falha vira rollback.
func (t *Transaction) Fail(code FailureCode, now time.Time) error {
	if t.status != PendingReference {
		return t.transitionError(Failed)
	}
	if code == "" {
		return fmt.Errorf("%w: falha exige código", ErrInvalidTransaction)
	}
	n := now.UTC()
	t.status, t.failureCode, t.updatedAt, t.completedAt = Failed, code, n, &n
	t.nextAttemptAt = nil
	return nil
}

// WaitForReference coloca a operação em espera pela referência, com a
// primeira retentativa em nextAttempt e prazo final em expiresAt.
func (t *Transaction) WaitForReference(nextAttempt, expiresAt, now time.Time) error {
	if t.status != Pending {
		return t.transitionError(PendingReference)
	}
	if !t.kind.AcceptsReference() || t.referenceExternalID == "" {
		return fmt.Errorf("%w: %s sem referência não espera", ErrInvalidTransaction, t.kind)
	}
	n, e := nextAttempt.UTC(), expiresAt.UTC()
	t.status, t.nextAttemptAt, t.expiresAt, t.updatedAt = PendingReference, &n, &e, now.UTC()
	return nil
}

// ScheduleRetry registra mais uma tentativa sem sucesso e agenda a próxima.
func (t *Transaction) ScheduleRetry(nextAttempt, now time.Time) error {
	if t.status != PendingReference {
		return t.transitionError(PendingReference)
	}
	n := nextAttempt.UTC()
	t.attempts++
	t.nextAttemptAt, t.updatedAt = &n, now.UTC()
	return nil
}

func (t *Transaction) finish(s Status, code FailureCode, result money.Money, ref *uuid.UUID, now time.Time) {
	n := now.UTC()
	t.status, t.failureCode, t.resultBalance = s, code, &result
	if ref != nil {
		r := *ref
		t.referenceTxID = &r
	}
	if t.status != PendingReference {
		t.nextAttemptAt = nil
	}
	t.updatedAt, t.completedAt = n, &n
}

// --- leitura -----------------------------------------------------------------

func (t *Transaction) ID() uuid.UUID                { return t.id }
func (t *Transaction) Origin() Origin               { return t.origin }
func (t *Transaction) Kind() Kind                   { return t.kind }
func (t *Transaction) Status() Status               { return t.status }
func (t *Transaction) WalletID() uuid.UUID          { return t.walletID }
func (t *Transaction) PlayerID() uuid.UUID          { return t.playerID }
func (t *Transaction) Money() money.Money           { return t.money }
func (t *Transaction) ProviderID() string           { return t.providerID }
func (t *Transaction) ExternalID() string           { return t.externalID }
func (t *Transaction) IdempotencyKey() string       { return t.idempotencyKey }
func (t *Transaction) PayloadHash() []byte          { return bytes.Clone(t.payloadHash) }
func (t *Transaction) RoundID() string              { return t.roundID }
func (t *Transaction) GameID() string               { return t.gameID }
func (t *Transaction) ReferenceExternalID() string  { return t.referenceExternalID }
func (t *Transaction) ReferenceTxID() *uuid.UUID    { return copyPtr(t.referenceTxID) }
func (t *Transaction) FailureCode() FailureCode     { return t.failureCode }
func (t *Transaction) ResultBalance() *money.Money  { return copyPtr(t.resultBalance) }
func (t *Transaction) Attempts() int                { return t.attempts }
func (t *Transaction) NextAttemptAt() *time.Time    { return copyPtr(t.nextAttemptAt) }
func (t *Transaction) ExpiresAt() *time.Time        { return copyPtr(t.expiresAt) }
func (t *Transaction) CreatedAt() time.Time         { return t.createdAt }
func (t *Transaction) UpdatedAt() time.Time         { return t.updatedAt }
func (t *Transaction) CompletedAt() *time.Time      { return copyPtr(t.completedAt) }
func (t *Transaction) SamePayload(hash []byte) bool { return bytes.Equal(t.payloadHash, hash) }

// copyPtr devolve uma cópia, para que quem lê não altere o estado interno.
func copyPtr[T any](p *T) *T {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

// RehydrateParams são as colunas persistidas de uma operação.
type RehydrateParams struct {
	ID                  uuid.UUID
	Origin              Origin
	Kind                Kind
	Status              Status
	WalletID            uuid.UUID
	PlayerID            uuid.UUID
	Money               money.Money
	ProviderID          string
	ExternalID          string
	IdempotencyKey      string
	PayloadHash         []byte
	RoundID             string
	GameID              string
	ReferenceExternalID string
	ReferenceTxID       *uuid.UUID
	FailureCode         FailureCode
	ResultBalance       *money.Money
	Attempts            int
	NextAttemptAt       *time.Time
	ExpiresAt           *time.Time
	CreatedAt           time.Time
	UpdatedAt           time.Time
	CompletedAt         *time.Time
}

// Rehydrate reconstrói uma operação persistida sem reaplicar transições nem
// emitir eventos; só confere que o estado é coerente.
func Rehydrate(p RehydrateParams) (*Transaction, error) {
	if p.ID == uuid.Nil || p.WalletID == uuid.Nil || !p.Money.Valid() {
		return nil, fmt.Errorf("%w: reidratação sem id, carteira ou valor", ErrInvalidTransaction)
	}
	switch p.Status {
	case Pending, PendingReference, Processed, Rejected, Failed:
	default:
		return nil, fmt.Errorf("%w: estado %q", ErrInvalidTransaction, p.Status)
	}
	if (p.Status == Rejected || p.Status == Failed) != (p.FailureCode != "") {
		return nil, fmt.Errorf("%w: failure_code incoerente com %s", ErrInvalidTransaction, p.Status)
	}
	utc := func(t *time.Time) *time.Time {
		if t == nil {
			return nil
		}
		v := t.UTC()
		return &v
	}
	return &Transaction{
		id: p.ID, origin: p.Origin, kind: p.Kind, status: p.Status,
		walletID: p.WalletID, playerID: p.PlayerID, money: p.Money,
		providerID: p.ProviderID, externalID: p.ExternalID, idempotencyKey: p.IdempotencyKey,
		payloadHash: bytes.Clone(p.PayloadHash), roundID: p.RoundID, gameID: p.GameID,
		referenceExternalID: p.ReferenceExternalID, referenceTxID: copyPtr(p.ReferenceTxID),
		failureCode: p.FailureCode, resultBalance: copyPtr(p.ResultBalance),
		attempts: p.Attempts, nextAttemptAt: utc(p.NextAttemptAt), expiresAt: utc(p.ExpiresAt),
		createdAt: p.CreatedAt.UTC(), updatedAt: p.UpdatedAt.UTC(), completedAt: utc(p.CompletedAt),
	}, nil
}
