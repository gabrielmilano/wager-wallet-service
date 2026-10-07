package httpapi

import (
	"net/http"
)

// NewRouter monta as rotas da API. Por enquanto só a liveness; as demais
// rotas entram a partir da Fase 07 e a readiness na Fase 11.
func NewRouter() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", live)
	return mux
}

// live responde 200 enquanto o processo consegue atender requisições. Não
// verifica dependências: isso é papel da readiness.
func live(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}
