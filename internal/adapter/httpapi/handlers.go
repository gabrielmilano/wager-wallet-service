package httpapi

import (
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/gabrielmilano/wager-wallet-service/internal/adapter/oidc"
	"github.com/gabrielmilano/wager-wallet-service/internal/app/apperr"
	"github.com/gabrielmilano/wager-wallet-service/internal/app/wagering"
	"github.com/gabrielmilano/wager-wallet-service/internal/app/wallets"
	"github.com/gabrielmilano/wager-wallet-service/internal/domain/money"
	"github.com/gabrielmilano/wager-wallet-service/internal/domain/wager"
	"github.com/gabrielmilano/wager-wallet-service/internal/domain/wallet"
)

// --- operações de provedores -------------------------------------------------

type transactionRequest struct {
	ProviderID                     string      `json:"providerId"`
	ExternalTransactionID          string      `json:"externalTransactionId"`
	PlayerID                       string      `json:"playerId"`
	WalletID                       string      `json:"walletId"`
	RoundID                        string      `json:"roundId"`
	GameID                         string      `json:"gameId"`
	Kind                           string      `json:"kind"`
	Money                          money.Money `json:"money"`
	ReferenceExternalTransactionID string      `json:"referenceExternalTransactionId,omitempty"`
}

// transactionResult é a resposta do envio de operação.
type transactionResult struct {
	TransactionID    uuid.UUID    `json:"transactionId"`
	Status           string       `json:"status"`
	Balance          *money.Money `json:"balance,omitempty"`
	FailureCode      string       `json:"failureCode,omitempty"`
	IdempotentReplay bool         `json:"idempotentReplay"`
}

// resultStatus: PROCESSED 200; PENDING_REFERENCE 202; REJECTED (e FAILED,
// visto num replay) 422. O replay devolve o mesmo status do original.
func resultStatus(s wager.Status) int {
	switch s {
	case wager.Processed:
		return http.StatusOK
	case wager.PendingReference:
		return http.StatusAccepted
	default:
		return http.StatusUnprocessableEntity
	}
}

func (h *handlers) postTransaction(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	if p.ProviderID == "" {
		h.writeError(w, r, forbidden("token de provedor sem provider_id"))
		return
	}

	var req transactionRequest
	if err := decodeJSON(w, r, &req); err != nil {
		h.writeError(w, r, err)
		return
	}
	// O providerId autorizado vem do token; o do corpo só é conferido.
	if req.ProviderID != "" && req.ProviderID != p.ProviderID {
		h.writeError(w, r, forbidden("providerId do corpo difere da identidade autenticada"))
		return
	}

	cmd, err := wagering.NewCommand(wagering.Input{
		ProviderID: p.ProviderID, ExternalTransactionID: req.ExternalTransactionID,
		PlayerID: req.PlayerID, WalletID: req.WalletID, RoundID: req.RoundID, GameID: req.GameID,
		Kind: req.Kind, Money: req.Money, ReferenceExternalTransactionID: req.ReferenceExternalTransactionID,
	})
	if err != nil {
		h.writeError(w, r, err)
		return
	}

	started := time.Now()
	res, err := h.wagering.Process(r.Context(), cmd, wagering.Metadata{
		IdempotencyKey: r.Header.Get("Idempotency-Key"),
		CorrelationID:  correlationFrom(r.Context()),
	})
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	tx := res.Transaction
	h.metrics.ObserveOperation("http", string(tx.Kind()), string(tx.Status()), res.Replay, time.Since(started))
	h.log.Info("operação registrada",
		slog.String("correlationId", correlationFrom(r.Context())),
		slog.String("providerId", tx.ProviderID()),
		slog.String("transactionId", tx.ID().String()),
		slog.String("walletId", tx.WalletID().String()),
		slog.String("kind", string(tx.Kind())),
		slog.String("status", string(tx.Status())),
		slog.String("failureCode", string(tx.FailureCode())),
		slog.Bool("idempotentReplay", res.Replay))
	h.writeBody(w, r, resultStatus(tx.Status()), transactionResult{
		TransactionID:    tx.ID(),
		Status:           string(tx.Status()),
		Balance:          tx.ResultBalance(),
		FailureCode:      string(tx.FailureCode()),
		IdempotentReplay: res.Replay,
	})
}

// transactionView é a consulta de uma operação: permite acompanhar
// pendências e ver códigos de rejeição ou falha.
type transactionView struct {
	TransactionID                  uuid.UUID    `json:"transactionId"`
	Origin                         string       `json:"origin"`
	ProviderID                     string       `json:"providerId,omitempty"`
	ExternalTransactionID          string       `json:"externalTransactionId,omitempty"`
	WalletID                       uuid.UUID    `json:"walletId"`
	PlayerID                       uuid.UUID    `json:"playerId"`
	RoundID                        string       `json:"roundId,omitempty"`
	GameID                         string       `json:"gameId,omitempty"`
	Kind                           string       `json:"kind"`
	Money                          money.Money  `json:"money"`
	Status                         string       `json:"status"`
	Balance                        *money.Money `json:"balance,omitempty"`
	FailureCode                    string       `json:"failureCode,omitempty"`
	ReferenceExternalTransactionID string       `json:"referenceExternalTransactionId,omitempty"`
	ReferenceTransactionID         *uuid.UUID   `json:"referenceTransactionId,omitempty"`
	Attempts                       int          `json:"attempts"`
	NextAttemptAt                  *time.Time   `json:"nextAttemptAt,omitempty"`
	ExpiresAt                      *time.Time   `json:"expiresAt,omitempty"`
	CreatedAt                      time.Time    `json:"createdAt"`
	UpdatedAt                      time.Time    `json:"updatedAt"`
	CompletedAt                    *time.Time   `json:"completedAt,omitempty"`
}

func viewOf(tx *wager.Transaction) transactionView {
	return transactionView{
		TransactionID: tx.ID(), Origin: string(tx.Origin()),
		ProviderID: tx.ProviderID(), ExternalTransactionID: tx.ExternalID(),
		WalletID: tx.WalletID(), PlayerID: tx.PlayerID(), RoundID: tx.RoundID(), GameID: tx.GameID(),
		Kind: string(tx.Kind()), Money: tx.Money(), Status: string(tx.Status()),
		Balance: tx.ResultBalance(), FailureCode: string(tx.FailureCode()),
		ReferenceExternalTransactionID: tx.ReferenceExternalID(), ReferenceTransactionID: tx.ReferenceTxID(),
		Attempts: tx.Attempts(), NextAttemptAt: tx.NextAttemptAt(), ExpiresAt: tx.ExpiresAt(),
		CreatedAt: tx.CreatedAt(), UpdatedAt: tx.UpdatedAt(), CompletedAt: tx.CompletedAt(),
	}
}

func (h *handlers) getTransaction(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("transactionId"))
	if err != nil {
		h.writeError(w, r, apperr.ValidationError("transactionId deve ser um UUID", nil))
		return
	}
	tx, err := h.wagering.GetTransaction(r.Context(), id)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	// Provedor só vê as próprias operações; a de outro responde como
	// inexistente, para não revelar que existe.
	if !canSee(principalFrom(r.Context()), tx) {
		h.writeError(w, r, apperr.New(apperr.NotFound, apperr.CodeTransactionNotFound, "operação não encontrada", nil))
		return
	}
	h.writeBody(w, r, http.StatusOK, viewOf(tx))
}

func (h *handlers) getProviderTransaction(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	providerID := r.PathValue("providerId")
	if !p.HasRole(oidc.RoleWalletAdmin) && providerID != p.ProviderID {
		h.writeError(w, r, forbidden("consulta a transações de outro provedor"))
		return
	}
	tx, err := h.wagering.GetByExternalID(r.Context(), providerID, r.PathValue("externalTransactionId"))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	h.writeBody(w, r, http.StatusOK, viewOf(tx))
}

func canSee(p oidc.Principal, tx *wager.Transaction) bool {
	return p.HasRole(oidc.RoleWalletAdmin) || (p.ProviderID != "" && tx.ProviderID() == p.ProviderID)
}

func forbidden(message string) error {
	return apperr.New(apperr.Forbidden, apperr.CodeProviderForbidden, message, nil)
}

// --- carteiras (serviço interno) ---------------------------------------------

type openWalletRequest struct {
	PlayerID       string      `json:"playerId"`
	InitialBalance money.Money `json:"initialBalance"`
}

type walletView struct {
	ID        uuid.UUID   `json:"id"`
	PlayerID  uuid.UUID   `json:"playerId"`
	Balance   money.Money `json:"balance"`
	Version   int64       `json:"version"`
	CreatedAt time.Time   `json:"createdAt"`
	UpdatedAt time.Time   `json:"updatedAt"`
}

func walletViewOf(w *wallet.Wallet) walletView {
	return walletView{ID: w.ID(), PlayerID: w.PlayerID(), Balance: w.Balance(), Version: w.Version(),
		CreatedAt: w.CreatedAt(), UpdatedAt: w.UpdatedAt()}
}

func (h *handlers) postWallet(w http.ResponseWriter, r *http.Request) {
	var req openWalletRequest
	if err := decodeJSON(w, r, &req); err != nil {
		h.writeError(w, r, err)
		return
	}
	wl, err := h.wallets.Open(r.Context(), wallets.OpenInput{
		PlayerID: req.PlayerID, InitialBalance: req.InitialBalance, CorrelationID: correlationFrom(r.Context()),
	})
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	w.Header().Set("Location", "/wallets/"+wl.ID().String())
	h.writeBody(w, r, http.StatusCreated, walletViewOf(wl))
}

func (h *handlers) getWallet(w http.ResponseWriter, r *http.Request) {
	id, ok := h.walletID(w, r)
	if !ok {
		return
	}
	wl, err := h.wallets.Get(r.Context(), id)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	h.writeBody(w, r, http.StatusOK, walletViewOf(wl))
}

type ledgerEntryView struct {
	ID            uuid.UUID   `json:"id"`
	TransactionID uuid.UUID   `json:"transactionId"`
	WalletVersion int64       `json:"walletVersion"`
	Direction     string      `json:"direction"`
	Money         money.Money `json:"money"`
	BalanceBefore money.Money `json:"balanceBefore"`
	BalanceAfter  money.Money `json:"balanceAfter"`
	CreatedAt     time.Time   `json:"createdAt"`
}

type ledgerPageView struct {
	WalletID   uuid.UUID         `json:"walletId"`
	Entries    []ledgerEntryView `json:"entries"`
	NextCursor string            `json:"nextCursor,omitempty"`
}

func (h *handlers) getLedger(w http.ResponseWriter, r *http.Request) {
	id, ok := h.walletID(w, r)
	if !ok {
		return
	}
	limit := 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			h.writeError(w, r, apperr.ValidationError("limit deve ser um inteiro", nil))
			return
		}
		limit = n
	}
	page, err := h.wallets.Ledger(r.Context(), id, r.URL.Query().Get("cursor"), limit)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	view := ledgerPageView{WalletID: id, Entries: make([]ledgerEntryView, 0, len(page.Entries)), NextCursor: page.NextCursor}
	for _, e := range page.Entries {
		view.Entries = append(view.Entries, ledgerEntryView{
			ID: e.ID(), TransactionID: e.TransactionID(), WalletVersion: e.WalletVersion(),
			Direction: string(e.Direction()), Money: e.Amount(),
			BalanceBefore: e.BalanceBefore(), BalanceAfter: e.BalanceAfter(), CreatedAt: e.CreatedAt(),
		})
	}
	h.writeBody(w, r, http.StatusOK, view)
}

type reconciliationView struct {
	WalletID          uuid.UUID   `json:"walletId"`
	StoredBalance     money.Money `json:"storedBalance"`
	CalculatedBalance money.Money `json:"calculatedBalance"`
	Difference        money.Money `json:"difference"`
	Consistent        bool        `json:"consistent"`
	CheckedEntries    int         `json:"checkedEntries"`
}

// postReconciliation compara o saldo gravado com o reconstruído pelo extrato.
// Divergência vai para a resposta, para o log (ERROR) e para a métrica.
func (h *handlers) postReconciliation(w http.ResponseWriter, r *http.Request) {
	id, ok := h.walletID(w, r)
	if !ok {
		return
	}
	rec, err := h.wallets.Reconcile(r.Context(), id)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	if !rec.Consistent {
		h.log.Error("divergência na reconciliação", slog.String("correlationId", correlationFrom(r.Context())),
			slog.String("walletId", id.String()), slog.String("storedBalance", rec.StoredBalance.Amount()),
			slog.String("calculatedBalance", rec.CalculatedBalance.Amount()), slog.String("difference", rec.Difference.Amount()))
	}
	h.writeBody(w, r, http.StatusOK, reconciliationView{
		WalletID: rec.WalletID, StoredBalance: rec.StoredBalance, CalculatedBalance: rec.CalculatedBalance,
		Difference: rec.Difference, Consistent: rec.Consistent, CheckedEntries: rec.CheckedEntries,
	})
}

func (h *handlers) walletID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("walletId"))
	if err != nil {
		h.writeError(w, r, apperr.ValidationError("walletId deve ser um UUID", nil))
		return uuid.Nil, false
	}
	return id, true
}
