package wager

import (
	"github.com/google/uuid"

	"github.com/gabrielmilano/wager-wallet-service/internal/domain/wallet"
)

// Outcome é o resultado da resolução de uma referência.
type Outcome int

const (
	// Proceed: a referência é válida; aplicar a movimentação em Direction.
	Proceed Outcome = iota + 1
	// Wait: a referência ainda não chegou ou também está esperando a sua;
	// a operação fica em PENDING_REFERENCE.
	Wait
	// Reject: rejeição definitiva com Code.
	Reject
)

// Resolution diz o que fazer com uma operação que referencia outra.
type Resolution struct {
	Outcome     Outcome
	Code        FailureCode
	Direction   wallet.Direction
	ReferenceID uuid.UUID
}

// ResolveReference aplica as regras de referência (função pura). original é
// a operação encontrada por (providerId, referenceExternalTransactionId), ou
// nil se ainda não chegou; originalReversed informa se ela já tem uma
// reversão PROCESSED. Ordem das verificações:
//
//  1. original ausente                          -> Wait
//  2. tipo incompatível                         -> INVALID_REFERENCE_KIND
//  3. jogador/carteira/moeda/rodada diferentes  -> REFERENCE_CONTEXT_MISMATCH
//  4. original em PENDING_REFERENCE             -> Wait
//  5. original REJECTED ou FAILED               -> REFERENCE_NOT_PROCESSED
//  6. valor diferente (REFUND/ROLLBACK)         -> REFERENCE_AMOUNT_MISMATCH
//  7. já revertida (REFUND/ROLLBACK)            -> REFERENCE_ALREADY_REVERSED
//  8. caso contrário                            -> Proceed
//
// Tipo e contexto vêm antes do estado porque são imutáveis: uma referência de
// tipo errado é recusada já, sem esperar a original concluir. O provedor é o
// mesmo por construção (a busca é por provedor).
func ResolveReference(op, original *Transaction, originalReversed bool) Resolution {
	if original == nil {
		return Resolution{Outcome: Wait}
	}
	reject := func(code FailureCode) Resolution {
		return Resolution{Outcome: Reject, Code: code, ReferenceID: original.id}
	}

	direction, ok := referenceDirection(op.kind, original.kind)
	if !ok {
		return reject(InvalidReferenceKind)
	}
	if op.playerID != original.playerID || op.walletID != original.walletID ||
		op.money.Currency() != original.money.Currency() || op.roundID != original.roundID {
		return reject(ReferenceContextMismatch)
	}
	switch original.status {
	case Pending, PendingReference:
		return Resolution{Outcome: Wait}
	case Rejected, Failed:
		return reject(ReferenceNotProcessed)
	}
	if op.kind == Refund || op.kind == Rollback {
		if !op.money.Equal(original.money) {
			return reject(ReferenceAmountMismatch)
		}
		if originalReversed {
			return reject(ReferenceAlreadyReversed)
		}
	}
	return Resolution{Outcome: Proceed, Direction: direction, ReferenceID: original.id}
}

// referenceDirection devolve o sentido da movimentação de op sobre original,
// ou false se o par de tipos não é permitido:
//
//	REFUND   de BET            -> CREDIT (devolve a aposta)
//	ROLLBACK de BET            -> CREDIT (desfaz o débito)
//	ROLLBACK de WIN ou REFUND  -> DEBIT  (desfaz o crédito)
//	WIN      de BET            -> CREDIT (pagamento da aposta da rodada)
func referenceDirection(op, original Kind) (wallet.Direction, bool) {
	switch {
	case op == Refund && original == Bet:
		return wallet.Credit, true
	case op == Rollback && original == Bet:
		return wallet.Credit, true
	case op == Rollback && (original == Win || original == Refund):
		return wallet.Debit, true
	case op == Win && original == Bet:
		return wallet.Credit, true
	default:
		return "", false
	}
}

// DirectionWithoutReference é o sentido das operações que não dependem de
// referência: BET debita, WIN credita, LOSS não movimenta (false).
func DirectionWithoutReference(k Kind) (wallet.Direction, bool) {
	switch k {
	case Bet:
		return wallet.Debit, true
	case Win:
		return wallet.Credit, true
	default:
		return "", false
	}
}
