package wager

import (
	"errors"
	"fmt"
)

// Erros do pacote, classificáveis com errors.Is.
var (
	ErrInvalidTransaction = errors.New("wager: operação inválida")
	ErrInvalidKind        = errors.New("wager: tipo inválido")
	ErrInvalidTransition  = errors.New("wager: transição de estado inválida")
)

// Kind é o tipo da operação.
type Kind string

const (
	Opening  Kind = "OPENING" // interno: abertura de carteira
	Bet      Kind = "BET"
	Win      Kind = "WIN"
	Loss     Kind = "LOSS"
	Refund   Kind = "REFUND"
	Rollback Kind = "ROLLBACK"
)

// ParseExternalKind aceita os tipos que podem chegar por HTTP ou SQS.
// OPENING é reservado à abertura interna e é recusado.
func ParseExternalKind(s string) (Kind, error) {
	switch k := Kind(s); k {
	case Bet, Win, Loss, Refund, Rollback:
		return k, nil
	case Opening:
		return "", fmt.Errorf("%w: OPENING é reservado à abertura interna", ErrInvalidKind)
	default:
		return "", fmt.Errorf("%w: %q", ErrInvalidKind, s)
	}
}

// RequiresReference informa se o tipo exige referenceExternalTransactionId.
func (k Kind) RequiresReference() bool { return k == Refund || k == Rollback }

// AcceptsReference informa se o tipo pode trazer uma referência: obrigatória
// em REFUND e ROLLBACK, opcional em WIN, proibida em BET e LOSS.
func (k Kind) AcceptsReference() bool { return k == Win || k == Refund || k == Rollback }

// Status é o estado da operação.
type Status string

const (
	Pending          Status = "PENDING"
	PendingReference Status = "PENDING_REFERENCE"
	Processed        Status = "PROCESSED"
	Rejected         Status = "REJECTED"
	Failed           Status = "FAILED"
)

// IsTerminal informa se o estado é final (não admite novas transições).
func (s Status) IsTerminal() bool { return s == Processed || s == Rejected || s == Failed }

// Origin distingue a abertura interna das operações de provedores.
type Origin string

const (
	Internal Origin = "INTERNAL"
	External Origin = "EXTERNAL"
)

// FailureCode é o código estável de uma rejeição definitiva (persistida como
// REJECTED) ou de uma falha permanente (FAILED).
type FailureCode string

// Catálogo de rejeições definitivas (notes/decisoes-aprovadas, ARCHITECTURE).
const (
	InsufficientFunds            FailureCode = "INSUFFICIENT_FUNDS"
	InsufficientFundsForReversal FailureCode = "INSUFFICIENT_FUNDS_FOR_REVERSAL"
	ReferenceNotFound            FailureCode = "REFERENCE_NOT_FOUND"
	ReferenceNotProcessed        FailureCode = "REFERENCE_NOT_PROCESSED"
	ReferenceAlreadyReversed     FailureCode = "REFERENCE_ALREADY_REVERSED"
	InvalidReferenceKind         FailureCode = "INVALID_REFERENCE_KIND"
	ReferenceAmountMismatch      FailureCode = "REFERENCE_AMOUNT_MISMATCH"
	ReferenceContextMismatch     FailureCode = "REFERENCE_CONTEXT_MISMATCH"
)

// InsufficientFundsCode devolve o código de saldo insuficiente adequado ao
// tipo: a reversão tem código próprio, diferente do da aposta.
func InsufficientFundsCode(k Kind) FailureCode {
	if k == Rollback {
		return InsufficientFundsForReversal
	}
	return InsufficientFunds
}
