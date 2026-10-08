package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/gabrielmilano/wager-wallet-service/internal/adapter/oidc"
)

// TokenVerifier valida o bearer token e devolve a identidade.
type TokenVerifier interface {
	Verify(ctx context.Context, raw string) (oidc.Principal, error)
}

type ctxKey int

const (
	principalKey ctxKey = iota
	correlationKey
)

func principalFrom(ctx context.Context) oidc.Principal {
	p, _ := ctx.Value(principalKey).(oidc.Principal)
	return p
}

func correlationFrom(ctx context.Context) string {
	c, _ := ctx.Value(correlationKey).(string)
	return c
}

// statusRecorder guarda o status escrito, para o log.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// observe aplica, de fora para dentro: correlationId, log da requisição,
// recuperação de pânico e prazo da requisição.
func (h *handlers) observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()

		correlation := r.Header.Get("X-Correlation-Id")
		if !validCorrelationID(correlation) {
			correlation = uuid.NewString()
		}
		w.Header().Set("X-Correlation-Id", correlation)

		ctx, cancel := context.WithTimeout(context.WithValue(r.Context(), correlationKey, correlation), h.requestTimeout)
		defer cancel()
		r = r.WithContext(ctx)
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		defer func() {
			if v := recover(); v != nil {
				h.log.Error("pânico no handler HTTP", slog.Any("panic", v), slog.String("correlationId", correlation))
				h.writeError(rec, r, errors.New("pânico no handler"))
			}
			attrs := []any{
				slog.String("method", r.Method),
				slog.String("route", r.Pattern),
				slog.Int("status", rec.status),
				slog.Int64("durationMs", time.Since(started).Milliseconds()),
				slog.String("correlationId", correlation),
			}
			if p := principalFrom(r.Context()); p.ClientID != "" {
				attrs = append(attrs, slog.String("clientId", p.ClientID), slog.String("providerId", p.ProviderID))
			}
			h.log.Info("requisição HTTP", attrs...)
		}()

		next.ServeHTTP(rec, r)
	})
}

// validCorrelationID aceita só identificadores curtos e imprimíveis, para não
// levar lixo do cliente para os logs.
func validCorrelationID(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for _, c := range s {
		if c < 0x21 || c > 0x7e {
			return false
		}
	}
	return true
}

// authorize exige um bearer token válido (401) com pelo menos uma das roles
// (403). A identidade vai para o contexto da requisição.
func (h *handlers) authorize(roles []string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || raw == "" {
			h.unauthorized(w, r, "token ausente")
			return
		}
		p, err := h.verifier.Verify(r.Context(), raw)
		if err != nil {
			h.log.Info("token recusado", slog.String("correlationId", correlationFrom(r.Context())), slog.Any("error", err))
			h.unauthorized(w, r, "token inválido ou expirado")
			return
		}
		allowed := false
		for _, role := range roles {
			if p.HasRole(role) {
				allowed = true
				break
			}
		}
		if !allowed {
			h.writeBody(w, r, http.StatusForbidden, errorBody{Code: codeForbidden, Message: "identidade sem permissão para esta operação"})
			return
		}
		ctx := context.WithValue(r.Context(), principalKey, p)
		*r = *r.WithContext(ctx)
		next(w, r)
	}
}

func (h *handlers) unauthorized(w http.ResponseWriter, r *http.Request, message string) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="wager-wallet-service"`)
	h.writeBody(w, r, http.StatusUnauthorized, errorBody{Code: codeUnauthorized, Message: message})
}
