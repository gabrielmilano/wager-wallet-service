// Package apperr classifica os erros que os casos de uso devolvem às
// entradas (HTTP e SQS). Cada classe tem um efeito definido na seção
// "Classificação de erros" do ARCHITECTURE.md; o mapeamento para status HTTP e
// ação SQS fica nos adapters.
package apperr

import (
	"errors"
	"fmt"
)

// Kind é a classe do erro.
type Kind int

const (
	// Validation: entrada corrigível; nada é gravado.
	Validation Kind = iota + 1
	// NotFound: recurso inexistente (ou de outro provedor).
	NotFound
	// Conflict: idempotência ou unicidade (409); nada é gravado.
	Conflict
	// Forbidden: identidade sem permissão para o recurso.
	Forbidden
	// Unavailable: falha transitória (banco fora, lock_timeout, resultado de
	// commit desconhecido); repetir é seguro.
	Unavailable
)

// Códigos estáveis de erros corrigíveis (catálogo em notes e ARCHITECTURE).
const (
	CodeValidation          = "VALIDATION_ERROR"
	CodeWalletNotFound      = "WALLET_NOT_FOUND"
	CodeWalletMismatch      = "WALLET_MISMATCH"
	CodeProviderForbidden   = "PROVIDER_FORBIDDEN"
	CodeIdempotencyConflict = "IDEMPOTENCY_CONFLICT"
	CodeWalletAlreadyExists = "WALLET_ALREADY_EXISTS"
	CodeTransactionNotFound = "TRANSACTION_NOT_FOUND"
	CodeMessageConflict     = "MESSAGE_CONFLICT"
	CodeUnavailable         = "SERVICE_UNAVAILABLE"
)

// Error é um erro classificado, com código estável e mensagem segura para o
// cliente (sem detalhes internos).
type Error struct {
	Kind    Kind
	Code    string
	Message string
	Err     error // causa, só para logs
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.Err)
	}
	return e.Code + ": " + e.Message
}

func (e *Error) Unwrap() error { return e.Err }

// As extrai o *Error de uma cadeia de erros.
func As(err error) (*Error, bool) {
	var e *Error
	ok := errors.As(err, &e)
	return e, ok
}

// IsKind informa se err é um *Error da classe k.
func IsKind(err error, k Kind) bool {
	e, ok := As(err)
	return ok && e.Kind == k
}

func New(kind Kind, code, message string, cause error) *Error {
	return &Error{Kind: kind, Code: code, Message: message, Err: cause}
}

func ValidationError(message string, cause error) *Error {
	return New(Validation, CodeValidation, message, cause)
}

func UnavailableError(cause error) *Error {
	return New(Unavailable, CodeUnavailable, "serviço temporariamente indisponível; tente novamente", cause)
}
