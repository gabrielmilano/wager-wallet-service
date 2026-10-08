//go:build integration

package integration

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// O exemplo do enunciado: abertura de 1000.00 e aposta de 25.00.
func TestReconciliationMatchesExample(t *testing.T) {
	walletID, playerID := openWalletHTTP(t, "1000.00")
	expectStatus(t, postOperation(t, token(t, "provider-a"), operation(walletID, playerID, "BET", "25.00", "")), http.StatusOK, "")

	r := call(t, "POST", "/wallets/"+walletID+"/reconciliation", token(t, "wallet-internal"), nil, nil)
	expectStatus(t, r, http.StatusOK, "")
	amount := func(k string) string { return r.body[k].(map[string]any)["amount"].(string) }
	if amount("storedBalance") != "975.00" || amount("calculatedBalance") != "975.00" || amount("difference") != "0.00" ||
		r.body["consistent"] != true || r.body["checkedEntries"] != float64(2) {
		t.Errorf("reconciliação = %s", r.rawBody)
	}

	// Só o serviço interno reconcilia.
	expectStatus(t, call(t, "POST", "/wallets/"+walletID+"/reconciliation", token(t, "provider-a"), nil, nil),
		http.StatusForbidden, "FORBIDDEN")
}

// Para criar uma divergência é preciso burlar as proteções do banco como
// superusuário (session_replication_role = replica desliga triggers e FKs
// na transação). A reconciliação detecta e reporta na resposta, no log e na
// métrica, sem corrigir nada.
func TestReconciliationReportsDivergence(t *testing.T) {
	ctx := testContext(t)
	walletID, _ := openWalletHTTP(t, "50.00")

	admin := connect(t, ctx, withDatabase(t, env("POSTGRES_ADMIN_URL",
		"postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable"), "wager_wallet"))
	err := pgx.BeginFunc(ctx, admin, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE wallets SET balance_minor = balance_minor + 100 WHERE id = $1`, walletID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	r := call(t, "POST", "/wallets/"+walletID+"/reconciliation", token(t, "wallet-internal"), nil, nil)
	expectStatus(t, r, http.StatusOK, "")
	if r.body["consistent"] != false || r.body["difference"].(map[string]any)["amount"] != "1.00" {
		t.Errorf("reconciliação = %s", r.rawBody)
	}
	// A reconciliação não altera o saldo.
	if got := walletBalance(t, walletID); got != "51.00" {
		t.Errorf("saldo = %s, want 51.00 (inalterado)", got)
	}
	if body := metricsBody(t, env("APP_BASE_URL", "http://localhost:8081")); !strings.Contains(body, `wallet_reconciliations_total{consistent="false"}`) {
		t.Error("métrica de divergência não registrada")
	}
}

func TestReadinessOnAllInstances(t *testing.T) {
	for _, base := range instances(t) {
		r := callAt(t, base, "GET", "/health/ready", "", nil, nil)
		expectStatus(t, r, http.StatusOK, "")
		checks, _ := r.body["checks"].(map[string]any)
		if r.str("status") != "ready" || checks["postgres"] != "ok" || checks["sqs"] != "ok" {
			t.Errorf("%s: readiness = %s", base, r.rawBody)
		}
	}
}

func metricsBody(t *testing.T, base string) string {
	t.Helper()
	resp, err := http.Get(base + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/metrics: %d", resp.StatusCode)
	}
	return string(body)
}

func TestMetricsAreExposed(t *testing.T) {
	walletID, playerID := openWalletHTTP(t, "10.00")
	base := env("APP_BASE_URL", "http://localhost:8081")
	op := operation(walletID, playerID, "BET", "1.00", "")
	expectStatus(t, postOperationAt(t, base, token(t, "provider-a"), op), http.StatusOK, "")
	expectStatus(t, postOperationAt(t, base, token(t, "provider-a"), op), http.StatusOK, "") // replay

	body := metricsBody(t, base)
	for _, want := range []string{
		`wager_operations_total{kind="BET",source="http",status="PROCESSED"}`,
		`wager_idempotent_replays_total{source="http"}`,
		`wager_processing_seconds_bucket`,
		`outbox_lag_seconds`,
		`outbox_pending_events`,
		`outbox_events_published_total`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("métrica ausente: %s", want)
		}
	}
}
