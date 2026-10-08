package httpapi

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/gabrielmilano/wager-wallet-service/internal/adapter/oidc"
	"github.com/gabrielmilano/wager-wallet-service/internal/app/wagering"
	"github.com/gabrielmilano/wager-wallet-service/internal/app/wallets"
)

// Deps são as dependências da API.
type Deps struct {
	Wagering *wagering.Service
	Wallets  *wallets.Service
	Verifier TokenVerifier
	Log      *slog.Logger
	// RequestTimeout é o prazo de cada requisição (padrão 10 s).
	RequestTimeout time.Duration
}

type handlers struct {
	wagering       *wagering.Service
	wallets        *wallets.Service
	verifier       TokenVerifier
	log            *slog.Logger
	requestTimeout time.Duration
}

// NewRouter monta as rotas. Matriz de autorização (roles de realm, ADR 0003):
//
//	rota                                                     provider        wallet-admin
//	POST /wagering/transactions                              sim (próprio)   não
//	GET  /wagering/transactions/{id}                         só as próprias  todas
//	GET  /providers/{providerId}/wagering/transactions/{ext} só o próprio    todos
//	POST /wallets, GET /wallets/{id}, GET /wallets/{id}/ledger não         sim
//	GET  /health/live                                        público         público
func NewRouter(d Deps) http.Handler {
	h := &handlers{
		wagering: d.Wagering, wallets: d.Wallets, verifier: d.Verifier,
		log: d.Log, requestTimeout: d.RequestTimeout,
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

	mux.HandleFunc("POST /wagering/transactions", h.authorize(provider, h.postTransaction))
	mux.HandleFunc("GET /wagering/transactions/{transactionId}", h.authorize(anyone, h.getTransaction))
	mux.HandleFunc("GET /providers/{providerId}/wagering/transactions/{externalTransactionId}",
		h.authorize(anyone, h.getProviderTransaction))

	mux.HandleFunc("POST /wallets", h.authorize(admin, h.postWallet))
	mux.HandleFunc("GET /wallets/{walletId}", h.authorize(admin, h.getWallet))
	mux.HandleFunc("GET /wallets/{walletId}/ledger", h.authorize(admin, h.getLedger))

	return h.observe(mux)
}

// live responde 200 enquanto o processo consegue atender requisições. Não
// verifica dependências: isso é papel da readiness.
func live(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}
