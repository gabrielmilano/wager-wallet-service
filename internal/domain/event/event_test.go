package event

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gabrielmilano/wager-wallet-service/internal/domain/money"
	"github.com/gabrielmilano/wager-wallet-service/internal/domain/wager"
	"github.com/gabrielmilano/wager-wallet-service/internal/domain/wallet"
)

// Instante em outro fuso: o evento precisa sair em UTC.
var occurred = time.Date(2026, 10, 7, 9, 30, 0, 0, time.FixedZone("BRT", -3*3600))

func meta() Meta {
	return Meta{EventID: uuid.New(), CorrelationID: "corr-1", OccurredAt: occurred}
}

func brl(t *testing.T, s string) money.Money {
	t.Helper()
	m, err := money.Parse(s, "BRL")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestOpeningEvents(t *testing.T) {
	w, entry, err := wallet.Open(wallet.OpenParams{
		ID: uuid.New(), PlayerID: uuid.New(), Initial: brl(t, "1000.00"),
		OpeningTxID: uuid.New(), EntryID: uuid.New(), Now: occurred,
	})
	if err != nil {
		t.Fatal(err)
	}
	opening, err := wager.NewOpening(entry.TransactionID(), w.ID(), w.PlayerID(), brl(t, "1000.00"), occurred)
	if err != nil {
		t.Fatal(err)
	}

	processed, err := NewWagerTransactionProcessed(meta(), opening)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := NewWalletBalanceChanged(meta(), *entry)
	if err != nil {
		t.Fatal(err)
	}

	if processed.EventType != TypeWagerTransactionProcessed || processed.Version != 1 || processed.AggregateID != w.ID() {
		t.Errorf("envelope Processed incorreto: %+v", processed)
	}
	if changed.EventType != TypeWalletBalanceChanged || changed.Version != 1 {
		t.Errorf("envelope BalanceChanged incorreto: %+v", changed)
	}

	// Abertura interna: sem metadados de provedor no payload.
	data, _ := json.Marshal(processed)
	for _, field := range []string{"providerId", "externalTransactionId", "roundId", "gameId", "causationId"} {
		if strings.Contains(string(data), field) {
			t.Errorf("OPENING não deveria ter %s: %s", field, data)
		}
	}

	var got map[string]any
	_ = json.Unmarshal(mustJSON(t, changed), &got)
	d := got["data"].(map[string]any)
	if got["occurredAt"] != "2026-10-07T12:30:00Z" {
		t.Errorf("occurredAt = %v, want UTC RFC 3339", got["occurredAt"])
	}
	want := map[string]any{
		"direction":     "CREDIT",
		"walletVersion": float64(1),
		"money":         map[string]any{"amount": "1000.00", "currency": "BRL"},
		"balanceBefore": map[string]any{"amount": "0.00", "currency": "BRL"},
		"balanceAfter":  map[string]any{"amount": "1000.00", "currency": "BRL"},
	}
	for k, v := range want {
		if !jsonEqual(d[k], v) {
			t.Errorf("data.%s = %v, want %v", k, d[k], v)
		}
	}
}

func TestTransactionEventsRequireMatchingStatus(t *testing.T) {
	tx, err := wager.NewExternal(wager.ExternalParams{
		ID: uuid.New(), WalletID: uuid.New(), PlayerID: uuid.New(), ProviderID: "provider-a",
		ExternalID: "bet-1", IdempotencyKey: "provider-a:bet-1", PayloadHash: []byte{1},
		RoundID: "r", GameID: "g", Kind: wager.Bet, Money: brl(t, "25.00"), Now: occurred,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewWagerTransactionProcessed(meta(), tx); !errors.Is(err, ErrInvalidEvent) {
		t.Errorf("Processed de PENDING: %v", err)
	}
	if _, err := NewWagerTransactionRejected(meta(), tx); !errors.Is(err, ErrInvalidEvent) {
		t.Errorf("Rejected de PENDING: %v", err)
	}

	_ = tx.Reject(wager.InsufficientFunds, brl(t, "10.00"), nil, occurred)
	m := meta()
	m.CausationID = "msg-123"
	ev, err := NewWagerTransactionRejected(m, tx)
	if err != nil {
		t.Fatal(err)
	}
	data := string(mustJSON(t, ev))
	for _, want := range []string{`"failureCode":"INSUFFICIENT_FUNDS"`, `"causationId":"msg-123"`,
		`"balance":{"amount":"10.00","currency":"BRL"}`, `"providerId":"provider-a"`} {
		if !strings.Contains(data, want) {
			t.Errorf("evento sem %s: %s", want, data)
		}
	}
}

func TestEnvelopeRequiresMeta(t *testing.T) {
	_, entry, _ := wallet.Open(wallet.OpenParams{
		ID: uuid.New(), PlayerID: uuid.New(), Initial: brl(t, "1.00"),
		OpeningTxID: uuid.New(), EntryID: uuid.New(), Now: occurred,
	})
	for name, m := range map[string]Meta{
		"sem eventId":       {CorrelationID: "c", OccurredAt: occurred},
		"sem correlationId": {EventID: uuid.New(), OccurredAt: occurred},
		"sem occurredAt":    {EventID: uuid.New(), CorrelationID: "c"},
	} {
		if _, err := NewWalletBalanceChanged(m, *entry); !errors.Is(err, ErrInvalidEvent) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
