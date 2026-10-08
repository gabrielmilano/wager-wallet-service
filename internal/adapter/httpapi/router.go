package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/gabrielmilano/wager-wallet-service/internal/adapter/oidc"
	"github.com/gabrielmilano/wager-wallet-service/internal/app/wagering"
	"github.com/gabrielmilano/wager-wallet-service/internal/app/wallets"
	"github.com/gabrielmilano/wager-wallet-service/internal/platform/metrics"
)

// Deps são as dependências da API.
type Deps struct {
	Wagering *wagering.Service
	Wallets  *wallets.Service
	Verifier TokenVerifier
	Log      *slog.Logger
	// RequestTimeout é o prazo de cada requisição (padrão 10 s).
	RequestTimeout time.Duration
	// Metrics é opcional (nil desliga /metrics e as medições).
	Metrics *metrics.Metrics
	// Readiness são as dependências verificadas por /health/ready.
	Readiness []ReadinessCheck
}

// ReadinessCheck verifica uma dependência (PostgreSQL, SQS).
type ReadinessCheck struct {
	Name  string
	Check func(ctx context.Context) error
}

type handlers struct {
	wagering       *wagering.Service
	wallets        *wallets.Service
	verifier       TokenVerifier
	log            *slog.Logger
	requestTimeout time.Duration
	metrics        *metrics.Metrics
	readiness      []ReadinessCheck
}

// NewRouter monta as rotas. Matriz de autorização (roles de realm, ADR 0003):
//
//	rota                                                     provider        wallet-admin
//	POST /wagering/transactions                              sim (próprio)   não
//	GET  /wagering/transactions/{id}                         só as próprias  todas
//	GET  /providers/{providerId}/wagering/transactions/{ext} só o próprio    todos
//	POST /wallets, GET /wallets/{id}, GET /wallets/{id}/ledger,
//	POST /wallets/{id}/reconciliation                        não             sim
//	GET  /health/live, /health/ready, /metrics               público         público
func NewRouter(d Deps) http.Handler {
	h := &handlers{
		wagering: d.Wagering, wallets: d.Wallets, verifier: d.Verifier,
		log: d.Log, requestTimeout: d.RequestTimeout, metrics: d.Metrics, readiness: d.Readiness,
	}
	if h.log == nil {
		h.log = slog.New(slog.DiscardHandler)
	}
	if h.requestTimeout <= 0 {
		h.requestTimeout = 10 * time.Second
	}

	provider := []string{oidc.RoleProvider}
	admin := []string{oidc.RoleWalletAdmin}
	anyone := []string{oidc.RoleProvider, oidc.RoleWalletAdmin}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", live)
	mux.HandleFunc("GET /health/ready", h.ready)
	if d.Metrics != nil {
		mux.Handle("GET /metrics", d.Metrics.Handler())
	}

	mux.HandleFunc("POST /wagering/transactions", h.authorize(provider, h.postTransaction))
	mux.HandleFunc("GET /wagering/transactions/{transactionId}", h.authorize(anyone, h.getTransaction))
	mux.HandleFunc("GET /providers/{providerId}/wagering/transactions/{externalTransactionId}",
		h.authorize(anyone, h.getProviderTransaction))

	mux.HandleFunc("POST /wallets", h.authorize(admin, h.postWallet))
	mux.HandleFunc("GET /wallets/{walletId}", h.authorize(admin, h.getWallet))
	mux.HandleFunc("GET /wallets/{walletId}/ledger", h.authorize(admin, h.getLedger))
	mux.HandleFunc("POST /wallets/{walletId}/reconciliation", h.authorize(admin, h.postReconciliation))

	return h.observe(mux)
}

// ready responde 200 se todas as dependências respondem (PostgreSQL e SQS),
// ou 503 com o nome das que falharam (o detalhe vai só para o log).
func (h *handlers) ready(w http.ResponseWriter, r *http.Request) {
	checks := map[string]string{}
	status := http.StatusOK
	for _, c := range h.readiness {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		err := c.Check(ctx)
		cancel()
		if err != nil {
			status = http.StatusServiceUnavailable
			checks[c.Name] = "fail"
			h.log.Warn("readiness: dependência indisponível", slog.String("dependency", c.Name), slog.Any("error", err))
			continue
		}
		checks[c.Name] = "ok"
	}
	body := map[string]any{"status": "ready", "checks": checks}
	if status != http.StatusOK {
		body["status"] = "not_ready"
	}
	h.writeBody(w, r, status, body)
}

// live responde 200 enquanto o processo consegue atender requisições. Não
// verifica dependências: isso é papel da readiness.
func live(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}
