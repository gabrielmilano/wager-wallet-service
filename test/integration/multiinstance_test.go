//go:build integration

package integration

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Cenários com as três réplicas do Compose (processos, conexões e memória
// independentes). Toda a coordenação entre elas é feita no PostgreSQL.

// instances devolve as URLs das réplicas e exige que todas estejam no ar.
func instances(t *testing.T) []string {
	t.Helper()
	urls := strings.Split(env("APP_INSTANCE_URLS", "http://localhost:8081,http://localhost:8082,http://localhost:8083"), ",")
	if len(urls) < 3 {
		t.Fatalf("APP_INSTANCE_URLS tem %d instâncias; os cenários exigem pelo menos 3", len(urls))
	}
	for _, u := range urls {
		resp, err := http.Get(u + "/health/live")
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("instância %s fora do ar (make up sobe as três réplicas): %v", u, err)
		}
		resp.Body.Close()
	}
	return urls
}

func postOperationAt(t *testing.T, base, bearer string, op map[string]any) apiResponse {
	t.Helper()
	key := "provider-a:" + op["externalTransactionId"].(string)
	return callAt(t, base, "POST", "/wagering/transactions", bearer, map[string]string{"Idempotency-Key": key}, op)
}

// debits conta os débitos do extrato de uma carteira.
func debits(t *testing.T, ctx context.Context, u *usecases, walletID string) int {
	t.Helper()
	return u.count(t, ctx, `SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id = $1 AND direction = 'DEBIT'`, walletID)
}

// Teste obrigatório: carteira com 100.00 recebe, ao mesmo tempo, duas apostas
// distintas de 80.00, cada uma numa instância diferente.
func TestMultiInstanceTwoBetsOf80On100(t *testing.T) {
	ctx := testContext(t)
	urls := instances(t)
	u := newUsecases(t, ctx, 3*time.Second)
	provider := token(t, "provider-a")

	for round := range 5 {
		walletID, playerID := openWalletHTTP(t, "100.00")
		results := make([]apiResponse, 2)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := range results {
			op := operation(walletID, playerID, "BET", "80.00", "")
			base := urls[(round+i)%len(urls)]
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				results[i] = postOperationAt(t, base, provider, op)
			}()
		}
		close(start)
		wg.Wait()

		statuses := []int{results[0].status, results[1].status}
		slices.Sort(statuses)
		if !slices.Equal(statuses, []int{http.StatusOK, http.StatusUnprocessableEntity}) {
			t.Fatalf("rodada %d: status %v, want 200 e 422 (%s | %s)", round, statuses, results[0].rawBody, results[1].rawBody)
		}
		for _, r := range results {
			if r.status == http.StatusUnprocessableEntity && r.str("failureCode") != "INSUFFICIENT_FUNDS" {
				t.Errorf("rodada %d: rejeição com %q", round, r.str("failureCode"))
			}
		}
		if got := walletBalance(t, walletID); got != "20.00" {
			t.Errorf("rodada %d: saldo %s, want 20.00", round, got)
		}
		if n := debits(t, ctx, u, walletID); n != 1 {
			t.Errorf("rodada %d: %d débitos, want 1", round, n)
		}
	}
}

// A mesma aposta enviada 50 vezes em paralelo, espalhada pelas três réplicas.
func TestMultiInstanceSameBetFiftyTimes(t *testing.T) {
	ctx := testContext(t)
	urls := instances(t)
	u := newUsecases(t, ctx, 3*time.Second)
	provider := token(t, "provider-a")
	walletID, playerID := openWalletHTTP(t, "100.00")
	op := operation(walletID, playerID, "BET", "25.00", "")

	var mu sync.Mutex
	ids := map[string]int{}
	fresh, statusErrors := 0, 0
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			r := postOperationAt(t, urls[i%len(urls)], provider, op)
			mu.Lock()
			defer mu.Unlock()
			if r.status != http.StatusOK {
				statusErrors++
				t.Errorf("envio %d: %d %s", i, r.status, r.rawBody)
				return
			}
			ids[r.str("transactionId")]++
			if r.body["idempotentReplay"] == false {
				fresh++
			}
		}()
	}
	close(start)
	wg.Wait()

	if len(ids) != 1 || fresh != 1 || statusErrors != 0 {
		t.Errorf("operações distintas %d, processamentos originais %d, erros %d; want 1, 1 e 0", len(ids), fresh, statusErrors)
	}
	if got := walletBalance(t, walletID); got != "75.00" {
		t.Errorf("saldo = %s, want 75.00", got)
	}
	if n := debits(t, ctx, u, walletID); n != 1 {
		t.Errorf("débitos = %d, want 1", n)
	}
}

// Carteiras distintas processadas ao mesmo tempo pelas três réplicas; no fim,
// o saldo de cada uma bate com o extrato.
func TestMultiInstanceManyWalletsInParallel(t *testing.T) {
	ctx := testContext(t)
	urls := instances(t)
	u := newUsecases(t, ctx, 3*time.Second)
	provider := token(t, "provider-a")

	type walletRef struct{ id, player string }
	var wallets []walletRef
	for range 12 {
		id, player := openWalletHTTP(t, "100.00")
		wallets = append(wallets, walletRef{id, player})
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	for wi, w := range wallets {
		for j := range 10 {
			kind, amount := "BET", "3.00"
			if j%2 == 1 {
				kind, amount = "WIN", "1.50"
			}
			op := operation(w.id, w.player, kind, amount, "")
			base := urls[(wi+j)%len(urls)]
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if r := postOperationAt(t, base, provider, op); r.status != http.StatusOK {
					t.Errorf("%s em %s: %d %s", kind, base, r.status, r.rawBody)
				}
			}()
		}
	}
	started := time.Now()
	close(start)
	wg.Wait()
	elapsed := time.Since(started)

	// 5 BETs de 3.00 e 5 WINs de 1.50 por carteira: 100 - 15 + 7.50.
	for _, w := range wallets {
		if got := walletBalance(t, w.id); got != "92.50" {
			t.Errorf("carteira %s: saldo %s, want 92.50", w.id, got)
		}
		u.reconcile(t, ctx, uuid.MustParse(w.id))
	}
	t.Logf("120 operações em 12 carteiras, 3 instâncias: %v", elapsed)
}

// A mesma operação chega por HTTP (numa réplica) e por SQS (consumida por
// qualquer uma das três) ao mesmo tempo: uma única movimentação.
func TestMultiInstanceHTTPAndSQSSameOperation(t *testing.T) {
	ctx := testContext(t)
	urls := instances(t)
	u := newUsecases(t, ctx, 3*time.Second)
	c := sqsClient(t, ctx)
	queue := inputQueueURL(t, ctx, c, env("SQS_INPUT_QUEUE_NAME", "wager-transactions.fifo"))
	provider := token(t, "provider-a")

	type pending struct{ walletID, messageID string }
	var cases []pending
	for i := range 5 {
		walletID, playerID := openWalletHTTP(t, "100.00")
		op := operation(walletID, playerID, "BET", "30.00", "")
		messageID := "msg-" + uuid.NewString()
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			send(t, ctx, c, queue, walletID, messageID, wagerMessage(messageID, sqsData(op)))
		}()
		go func() {
			defer wg.Done()
			if r := postOperationAt(t, urls[i%len(urls)], provider, op); r.status != http.StatusOK {
				t.Errorf("HTTP: %d %s", r.status, r.rawBody)
			}
		}()
		wg.Wait()
		cases = append(cases, pending{walletID, messageID})
	}

	for _, p := range cases {
		eventually(t, 20*time.Second, fmt.Sprintf("mensagem %s tratada", p.messageID), func() bool {
			return u.count(t, ctx, `SELECT count(*) FROM inbox_messages WHERE message_id = $1 AND processed_at IS NOT NULL`, p.messageID) == 1
		})
		if got := walletBalance(t, p.walletID); got != "70.00" {
			t.Errorf("carteira %s: saldo %s, want 70.00", p.walletID, got)
		}
		if n := debits(t, ctx, u, p.walletID); n != 1 {
			t.Errorf("carteira %s: %d débitos, want 1", p.walletID, n)
		}
	}
}
