package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gabrielmilano/wager-wallet-service/internal/app/apperr"
)

// Códigos de erro próprios da camada HTTP (os demais vêm de apperr).
const (
	codeUnauthorized = "UNAUTHORIZED"
	codeForbidden    = "FORBIDDEN"
	codeInternal     = "INTERNAL_ERROR"
)

// maxBodyBytes limita o corpo das requisições.
const maxBodyBytes = 1 << 20

// errorBody é o corpo de todas as respostas de erro.
type errorBody struct {
	Code          string `json:"code"`
	Message       string `json:"message"`
	CorrelationID string `json:"correlationId"`
}

func (h *handlers) writeBody(w http.ResponseWriter, r *http.Request, status int, body any) {
	if e, ok := body.(errorBody); ok {
		e.CorrelationID = correlationFrom(r.Context())
		body = e
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		h.log.Error("escrever resposta", slog.Any("error", err))
	}
}

// writeError converte o erro de um caso de uso em status HTTP:
//
//	Validation  -> 400 (VALIDATION_ERROR, WALLET_MISMATCH)
//	Forbidden   -> 403 (PROVIDER_FORBIDDEN)
//	NotFound    -> 404 (WALLET_NOT_FOUND, TRANSACTION_NOT_FOUND)
//	Conflict    -> 409 (IDEMPOTENCY_CONFLICT, WALLET_ALREADY_EXISTS)
//	Unavailable -> 503 + Retry-After (lock_timeout, banco fora)
//	outros      -> 500 INTERNAL_ERROR, sem detalhes (detalhe só no log)
func (h *handlers) writeError(w http.ResponseWriter, r *http.Request, err error) {
	e, ok := apperr.As(err)
	if !ok {
		h.log.Error("erro inesperado", slog.String("correlationId", correlationFrom(r.Context())), slog.Any("error", err))
		h.writeBody(w, r, http.StatusInternalServerError, errorBody{Code: codeInternal, Message: "erro interno"})
		return
	}

	status := http.StatusInternalServerError
	switch e.Kind {
	case apperr.Validation:
		status = http.StatusBadRequest
	case apperr.Forbidden:
		status = http.StatusForbidden
	case apperr.NotFound:
		status = http.StatusNotFound
	case apperr.Conflict:
		status = http.StatusConflict
		if e.Code == apperr.CodeIdempotencyConflict {
			h.metrics.Conflict("idempotency")
		}
	case apperr.Unavailable:
		status = http.StatusServiceUnavailable
		w.Header().Set("Retry-After", "1")
		h.metrics.Conflict(conflictReason(e))
		h.log.Warn("falha transitória", slog.String("correlationId", correlationFrom(r.Context())), slog.Any("error", e.Err))
	}
	h.writeBody(w, r, status, errorBody{Code: e.Code, Message: e.Message})
}

// decodeJSON lê exatamente um objeto JSON, recusando campos desconhecidos e
// corpos maiores que maxBodyBytes. Erros viram VALIDATION_ERROR.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return apperr.ValidationError("corpo excede 1 MiB", nil)
		}
		return apperr.ValidationError(fmt.Sprintf("JSON inválido: %v", err), nil)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return apperr.ValidationError("o corpo deve conter um único objeto JSON", nil)
	}
	return nil
}

// conflictReason classifica uma falha transitória para a métrica de
// conflitos de concorrência.
func conflictReason(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "55P03"):
		return "lock_timeout"
	case strings.Contains(msg, "40P01"):
		return "deadlock"
	case strings.Contains(msg, "40001"):
		return "serialization"
	default:
		return "unavailable"
	}
}
