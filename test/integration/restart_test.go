//go:build integration && restart

package integration

import (
	"net/http"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestRestartPreservesState reinicia as três réplicas (docker compose
// restart) e confere que idempotência, pendências e consistência financeira
// foram preservadas: nada disso vive em memória.
//
//	make test-restart
func TestRestartPreservesState(t *testing.T) {
	urls := instances(t)
	provider := token(t, "provider-a")
	walletID, playerID := openWalletHTTP(t, "100.00")

	// Antes: uma aposta concluída e um REFUND esperando uma aposta que ainda
	// não chegou.
	bet := operation(walletID, playerID, "BET", "25.00", "")
	first := postOperationAt(t, urls[0], provider, bet)
	expectStatus(t, first, http.StatusOK, "")
	lateBet := operation(walletID, playerID, "BET", "10.00", "")
	lateBet["externalTransactionId"] = "late-" + uuid.NewString()
	refund := operation(walletID, playerID, "REFUND", "10.00", lateBet["externalTransactionId"].(string))
	pending := postOperationAt(t, urls[1], provider, refund)
	expectStatus(t, pending, http.StatusAccepted, "")

	// Reinício de todas as instâncias.
	cmd := exec.Command("docker", "compose", "restart", "app", "app-2", "app-3")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("docker compose restart: %v", err)
	}
	eventually(t, 60*time.Second, "réplicas prontas depois do reinício", func() bool {
		for _, u := range urls {
			resp, err := http.Get(u + "/health/ready")
			if err != nil {
				return false
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				return false
			}
		}
		return true
	})

	// Idempotência: o replay devolve o resultado original.
	replay := postOperationAt(t, urls[2], provider, bet)
	expectStatus(t, replay, http.StatusOK, "")
	if replay.str("transactionId") != first.str("transactionId") || replay.body["idempotentReplay"] != true ||
		replay.body["balance"].(map[string]any)["amount"] != "75.00" {
		t.Errorf("replay depois do reinício = %s", replay.rawBody)
	}

	// Pendência: continua registrada e é resolvida quando a aposta chega.
	expectStatus(t, postOperationAt(t, urls[0], provider, lateBet), http.StatusOK, "")
	eventually(t, 30*time.Second, "pendência resolvida depois do reinício", func() bool {
		r := call(t, "GET", "/wagering/transactions/"+pending.str("transactionId"), provider, nil, nil)
		return r.str("status") == "PROCESSED"
	})

	// Consistência: 100 - 25 - 10 + 10.
	if got := walletBalance(t, walletID); got != "75.00" {
		t.Errorf("saldo = %s, want 75.00", got)
	}
	rec := call(t, "POST", "/wallets/"+walletID+"/reconciliation", token(t, "wallet-internal"), nil, nil)
	if rec.body["consistent"] != true {
		t.Errorf("reconciliação = %s", rec.rawBody)
	}
}
