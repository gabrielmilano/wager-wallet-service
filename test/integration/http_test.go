//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Testes ponta a ponta contra a aplicação do Compose (APP_BASE_URL), com
// tokens reais do Keycloak. A app valida os tokens buscando o JWKS pela rede
// interna (ADR 0009).

var (
	tokenMu    sync.Mutex
	tokenCache = map[string]string{}
)

// token obtém (e reaproveita) um access token via client_credentials.
func token(t *testing.T, clientID string) string {
	t.Helper()
	tokenMu.Lock()
	defer tokenMu.Unlock()
	if tok, ok := tokenCache[clientID]; ok {
		return tok
	}
	tok := fetchToken(t, clientID)
	tokenCache[clientID] = tok
	return tok
}

func fetchToken(t *testing.T, clientID string) string {
	t.Helper()
	issuer := env("OIDC_ISSUER_URL", "http://localhost:8180/realms/wager")
	secret := env(strings.ToUpper(strings.ReplaceAll(clientID, "-", "_"))+"_CLIENT_SECRET", clientID+"-secret")
	resp, err := http.PostForm(issuer+"/protocol/openid-connect/token", url.Values{
		"grant_type": {"client_credentials"}, "client_id": {clientID}, "client_secret": {secret},
	})
	if err != nil {
		t.Fatalf("token de %s: %v", clientID, err)
	}
	defer resp.Body.Close()
	var body struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || body.AccessToken == "" {
		t.Fatalf("token de %s: status %d, %v", clientID, resp.StatusCode, err)
	}
	return body.AccessToken
}

type apiResponse struct {
	status  int
	header  http.Header
	body    map[string]any
	rawBody string
}

func (r apiResponse) str(key string) string { s, _ := r.body[key].(string); return s }

// call faz uma requisição à API. body pode ser string (JSON cru) ou valor.
func call(t *testing.T, method, path, bearer string, headers map[string]string, body any) apiResponse {
	t.Helper()
	var reader io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		reader = strings.NewReader(b)
	default:
		data, _ := json.Marshal(b)
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, env("APP_BASE_URL", "http://localhost:8081")+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v (a app está no ar? make up)", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := apiResponse{status: resp.StatusCode, header: resp.Header, rawBody: string(raw)}
	_ = json.Unmarshal(raw, &out.body)
	return out
}

func expectStatus(t *testing.T, r apiResponse, status int, code string) {
	t.Helper()
	if r.status != status || (code != "" && r.str("code") != code) {
		t.Fatalf("status %d código %q, want %d %q; corpo: %s", r.status, r.str("code"), status, code, r.rawBody)
	}
}

// openWalletHTTP abre uma carteira em BRL pelo serviço interno.
func openWalletHTTP(t *testing.T, amount string) (walletID, playerID string) {
	t.Helper()
	playerID = uuid.NewString()
	r := call(t, "POST", "/wallets", token(t, "wallet-internal"), nil, map[string]any{
		"playerId": playerID, "initialBalance": map[string]string{"amount": amount, "currency": "BRL"},
	})
	expectStatus(t, r, http.StatusCreated, "")
	return r.str("id"), playerID
}

func operation(walletID, playerID, kind, amount, ref string) map[string]any {
	op := map[string]any{
		"providerId": "provider-a", "externalTransactionId": "http-" + uuid.NewString(),
		"playerId": playerID, "walletId": walletID, "roundId": "round-1", "gameId": "fortune-chimp",
		"kind": kind, "money": map[string]string{"amount": amount, "currency": "BRL"},
	}
	if ref != "" {
		op["referenceExternalTransactionId"] = ref
	}
	return op
}

func postOperation(t *testing.T, bearer string, op map[string]any) apiResponse {
	t.Helper()
	key := "provider-a:" + op["externalTransactionId"].(string)
	return call(t, "POST", "/wagering/transactions", bearer, map[string]string{"Idempotency-Key": key}, op)
}

func walletBalance(t *testing.T, walletID string) string {
	t.Helper()
	r := call(t, "GET", "/wallets/"+walletID, token(t, "wallet-internal"), nil, nil)
	expectStatus(t, r, http.StatusOK, "")
	return r.body["balance"].(map[string]any)["amount"].(string)
}

// --- autenticação ------------------------------------------------------------

func TestHTTPAuthentication(t *testing.T) {
	walletID, playerID := openWalletHTTP(t, "100.00")
	op := operation(walletID, playerID, "BET", "10.00", "")

	t.Run("sem token", func(t *testing.T) {
		r := postOperation(t, "", op)
		expectStatus(t, r, http.StatusUnauthorized, "UNAUTHORIZED")
		if !strings.HasPrefix(r.header.Get("WWW-Authenticate"), "Bearer") {
			t.Errorf("WWW-Authenticate = %q", r.header.Get("WWW-Authenticate"))
		}
	})
	t.Run("token malformado", func(t *testing.T) {
		expectStatus(t, postOperation(t, "nao-e-um-jwt", op), http.StatusUnauthorized, "UNAUTHORIZED")
	})
	t.Run("token adulterado", func(t *testing.T) {
		parts := strings.Split(token(t, "provider-b"), ".")
		forged := parts[0] + "." + strings.Split(token(t, "provider-a"), ".")[1] + "." + parts[2]
		expectStatus(t, postOperation(t, forged, op), http.StatusUnauthorized, "UNAUTHORIZED")
	})
	t.Run("token expirado", func(t *testing.T) {
		short := fetchToken(t, "provider-a-short") // vale 1 segundo
		time.Sleep(2500 * time.Millisecond)
		expectStatus(t, postOperation(t, short, op), http.StatusUnauthorized, "UNAUTHORIZED")
	})
	t.Run("nenhuma tentativa teve efeito financeiro", func(t *testing.T) {
		if got := walletBalance(t, walletID); got != "100.00" {
			t.Errorf("saldo = %s, want 100.00", got)
		}
		r := call(t, "GET", "/providers/provider-a/wagering/transactions/"+op["externalTransactionId"].(string),
			token(t, "provider-a"), nil, nil)
		expectStatus(t, r, http.StatusNotFound, "TRANSACTION_NOT_FOUND")
	})
}

// --- autorização -------------------------------------------------------------

func TestHTTPAuthorization(t *testing.T) {
	walletID, playerID := openWalletHTTP(t, "100.00")

	t.Run("provedor não acessa operações de carteira", func(t *testing.T) {
		body := map[string]any{"playerId": uuid.NewString(), "initialBalance": map[string]string{"amount": "1.00", "currency": "BRL"}}
		expectStatus(t, call(t, "POST", "/wallets", token(t, "provider-a"), nil, body), http.StatusForbidden, "FORBIDDEN")
		expectStatus(t, call(t, "GET", "/wallets/"+walletID, token(t, "provider-a"), nil, nil), http.StatusForbidden, "FORBIDDEN")
		expectStatus(t, call(t, "GET", "/wallets/"+walletID+"/ledger", token(t, "provider-a"), nil, nil), http.StatusForbidden, "FORBIDDEN")
	})

	t.Run("serviço interno não envia operação de provedor", func(t *testing.T) {
		r := postOperation(t, token(t, "wallet-internal"), operation(walletID, playerID, "BET", "1.00", ""))
		expectStatus(t, r, http.StatusForbidden, "FORBIDDEN")
	})

	// Operação do provider-a, para os testes de isolamento.
	op := operation(walletID, playerID, "BET", "10.00", "")
	created := postOperation(t, token(t, "provider-a"), op)
	expectStatus(t, created, http.StatusOK, "")
	txID := created.str("transactionId")
	ext := op["externalTransactionId"].(string)

	t.Run("provedor B não vê a operação de A (404, sem revelar)", func(t *testing.T) {
		r := call(t, "GET", "/wagering/transactions/"+txID, token(t, "provider-b"), nil, nil)
		expectStatus(t, r, http.StatusNotFound, "TRANSACTION_NOT_FOUND")
	})
	t.Run("provedor B não consulta pelo caminho de A (403)", func(t *testing.T) {
		r := call(t, "GET", "/providers/provider-a/wagering/transactions/"+ext, token(t, "provider-b"), nil, nil)
		expectStatus(t, r, http.StatusForbidden, "PROVIDER_FORBIDDEN")
	})
	t.Run("provedor B não reaproveita a operação de A num replay", func(t *testing.T) {
		r := call(t, "POST", "/wagering/transactions", token(t, "provider-b"),
			map[string]string{"Idempotency-Key": "provider-a:" + ext}, op)
		expectStatus(t, r, http.StatusForbidden, "PROVIDER_FORBIDDEN")
	})
	t.Run("provedor A vê a própria operação", func(t *testing.T) {
		r := call(t, "GET", "/wagering/transactions/"+txID, token(t, "provider-a"), nil, nil)
		expectStatus(t, r, http.StatusOK, "")
		if r.str("externalTransactionId") != ext || r.str("status") != "PROCESSED" {
			t.Errorf("consulta = %s", r.rawBody)
		}
		r = call(t, "GET", "/providers/provider-a/wagering/transactions/"+ext, token(t, "provider-a"), nil, nil)
		expectStatus(t, r, http.StatusOK, "")
	})
	t.Run("serviço interno vê qualquer operação", func(t *testing.T) {
		expectStatus(t, call(t, "GET", "/wagering/transactions/"+txID, token(t, "wallet-internal"), nil, nil), http.StatusOK, "")
	})

	t.Run("providerId do corpo diferente do token: 403 sem efeito", func(t *testing.T) {
		other := operation(walletID, playerID, "BET", "50.00", "")
		r := postOperation(t, token(t, "provider-b"), other) // corpo diz provider-a
		expectStatus(t, r, http.StatusForbidden, "PROVIDER_FORBIDDEN")
		if got := walletBalance(t, walletID); got != "90.00" {
			t.Errorf("saldo = %s, want 90.00", got)
		}
	})
}

// --- contrato das operações --------------------------------------------------

func TestHTTPOperationContract(t *testing.T) {
	walletID, playerID := openWalletHTTP(t, "100.00")
	provider := token(t, "provider-a")

	bet := operation(walletID, playerID, "BET", "25.00", "")
	r := postOperation(t, provider, bet)
	expectStatus(t, r, http.StatusOK, "")
	if r.str("status") != "PROCESSED" || r.body["idempotentReplay"] != false ||
		r.body["balance"].(map[string]any)["amount"] != "75.00" {
		t.Errorf("BET = %s", r.rawBody)
	}

	t.Run("replay devolve o resultado original", func(t *testing.T) {
		again := postOperation(t, provider, bet)
		expectStatus(t, again, http.StatusOK, "")
		if again.str("transactionId") != r.str("transactionId") || again.body["idempotentReplay"] != true {
			t.Errorf("replay = %s", again.rawBody)
		}
	})
	t.Run("mesma chave com outro corpo: 409", func(t *testing.T) {
		changed := map[string]any{}
		for k, v := range bet {
			changed[k] = v
		}
		changed["money"] = map[string]string{"amount": "26.00", "currency": "BRL"}
		key := "provider-a:" + bet["externalTransactionId"].(string)
		resp := call(t, "POST", "/wagering/transactions", provider, map[string]string{"Idempotency-Key": key}, changed)
		expectStatus(t, resp, http.StatusConflict, "IDEMPOTENCY_CONFLICT")
	})
	t.Run("rejeição de negócio: 422 com failureCode, também no replay", func(t *testing.T) {
		big := operation(walletID, playerID, "BET", "1000.00", "")
		first := postOperation(t, provider, big)
		expectStatus(t, first, http.StatusUnprocessableEntity, "")
		if first.str("status") != "REJECTED" || first.str("failureCode") != "INSUFFICIENT_FUNDS" {
			t.Errorf("rejeição = %s", first.rawBody)
		}
		replay := postOperation(t, provider, big)
		expectStatus(t, replay, http.StatusUnprocessableEntity, "")
		if replay.body["idempotentReplay"] != true {
			t.Errorf("replay da rejeição = %s", replay.rawBody)
		}
	})
	t.Run("referência ainda não chegou: 202", func(t *testing.T) {
		refund := operation(walletID, playerID, "REFUND", "5.00", "ainda-nao-chegou-"+uuid.NewString())
		resp := postOperation(t, provider, refund)
		expectStatus(t, resp, http.StatusAccepted, "")
		if resp.str("status") != "PENDING_REFERENCE" {
			t.Errorf("pendência = %s", resp.rawBody)
		}
	})
	t.Run("LOSS: 200 sem movimentação", func(t *testing.T) {
		expectStatus(t, postOperation(t, provider, operation(walletID, playerID, "LOSS", "0.00", "")), http.StatusOK, "")
	})

	t.Run("entradas inválidas: 400", func(t *testing.T) {
		cases := map[string]apiResponse{
			"amount numérico": call(t, "POST", "/wagering/transactions", provider, map[string]string{"Idempotency-Key": "k-" + uuid.NewString()},
				`{"providerId":"provider-a","externalTransactionId":"x","playerId":"`+playerID+`","walletId":"`+walletID+
					`","roundId":"r","gameId":"g","kind":"BET","money":{"amount":25.00,"currency":"BRL"}}`),
			"campo desconhecido": call(t, "POST", "/wagering/transactions", provider, map[string]string{"Idempotency-Key": "k-" + uuid.NewString()},
				`{"providerId":"provider-a","foo":1}`),
			"sem Idempotency-Key": call(t, "POST", "/wagering/transactions", provider, nil, operation(walletID, playerID, "BET", "1.00", "")),
			"OPENING externo":     postOperation(t, provider, operation(walletID, playerID, "OPENING", "1.00", "")),
			"valor negativo":      postOperation(t, provider, operation(walletID, playerID, "BET", "-1.00", "")),
			"escala excedente":    postOperation(t, provider, operation(walletID, playerID, "BET", "1.001", "")),
		}
		for name, resp := range cases {
			if resp.status != http.StatusBadRequest || resp.str("code") != "VALIDATION_ERROR" {
				t.Errorf("%s: %d %s", name, resp.status, resp.rawBody)
			}
		}
	})
	t.Run("carteira inexistente: 404; de outro jogador: 400", func(t *testing.T) {
		expectStatus(t, postOperation(t, provider, operation(uuid.NewString(), playerID, "BET", "1.00", "")),
			http.StatusNotFound, "WALLET_NOT_FOUND")
		expectStatus(t, postOperation(t, provider, operation(walletID, uuid.NewString(), "BET", "1.00", "")),
			http.StatusBadRequest, "WALLET_MISMATCH")
	})

	if got := walletBalance(t, walletID); got != "75.00" {
		t.Errorf("saldo final = %s, want 75.00", got)
	}
}

func TestHTTPWalletEndpoints(t *testing.T) {
	admin := token(t, "wallet-internal")
	playerID := uuid.NewString()
	open := call(t, "POST", "/wallets", admin, nil, map[string]any{
		"playerId": playerID, "initialBalance": map[string]string{"amount": "1000.00", "currency": "BRL"},
	})
	expectStatus(t, open, http.StatusCreated, "")
	if open.body["version"] != float64(1) || open.header.Get("Location") != "/wallets/"+open.str("id") {
		t.Errorf("abertura = %s", open.rawBody)
	}

	dup := call(t, "POST", "/wallets", admin, nil, map[string]any{
		"playerId": playerID, "initialBalance": map[string]string{"amount": "1.00", "currency": "BRL"},
	})
	expectStatus(t, dup, http.StatusConflict, "WALLET_ALREADY_EXISTS")

	ledger := call(t, "GET", "/wallets/"+open.str("id")+"/ledger?limit=10", admin, nil, nil)
	expectStatus(t, ledger, http.StatusOK, "")
	entries, _ := ledger.body["entries"].([]any)
	if len(entries) != 1 || entries[0].(map[string]any)["direction"] != "CREDIT" {
		t.Errorf("extrato = %s", ledger.rawBody)
	}

	expectStatus(t, call(t, "GET", "/wallets/"+uuid.NewString(), admin, nil, nil), http.StatusNotFound, "WALLET_NOT_FOUND")
	expectStatus(t, call(t, "GET", "/wallets/nao-e-uuid", admin, nil, nil), http.StatusBadRequest, "VALIDATION_ERROR")
}

func TestHTTPCorrelationID(t *testing.T) {
	r := call(t, "GET", "/wallets/"+uuid.NewString(), token(t, "wallet-internal"),
		map[string]string{"X-Correlation-Id": "corr-123"}, nil)
	if r.header.Get("X-Correlation-Id") != "corr-123" || r.str("correlationId") != "corr-123" {
		t.Errorf("correlationId não propagado: header %q, corpo %s", r.header.Get("X-Correlation-Id"), r.rawBody)
	}
}
