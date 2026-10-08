package sqs

import (
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	valid := `{"messageId":"msg-123","type":"WagerTransactionRequested","occurredAt":"2026-09-08T12:00:00.000Z",
	  "data":{"providerId":"provider-a","externalTransactionId":"transaction-123","idempotencyKey":"provider-a:transaction-123",
	  "playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1","walletId":"0192f291-27dd-7d3f-8071-5f8685deef37",
	  "roundId":"round-987","gameId":"fortune-chimp","kind":"BET","money":{"amount":"25.00","currency":"BRL"}}}`
	env, data, err := parse(valid)
	if err != nil {
		t.Fatal(err)
	}
	if env.MessageID != "msg-123" || data.IdempotencyKey != "provider-a:transaction-123" || data.Money.Minor() != 2500 {
		t.Errorf("parse = %+v %+v", env, data)
	}

	tests := map[string]string{
		"não é JSON":          `isso não é json`,
		"sem messageId":       `{"type":"WagerTransactionRequested","data":{}}`,
		"tipo desconhecido":   `{"messageId":"m","type":"Outro","data":{}}`,
		"sem data":            `{"messageId":"m","type":"WagerTransactionRequested"}`,
		"campo extra em data": strings.Replace(valid, `"kind":"BET"`, `"kind":"BET","foo":1`, 1),
		"amount numérico":     strings.Replace(valid, `"amount":"25.00"`, `"amount":25.00`, 1),
	}
	for name, body := range tests {
		if _, _, err := parse(body); err == nil {
			t.Errorf("%s: esperado erro", name)
		}
	}
}
