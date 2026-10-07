package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"
)

func TestNewWritesJSONWithServiceFields(t *testing.T) {
	var buf bytes.Buffer
	log := New(&buf, slog.LevelInfo, "local-1")

	log.Info("iniciado", slog.String("walletId", "w-1"))

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("saída não é JSON: %v (%q)", err, buf.String())
	}
	want := map[string]string{
		"level":      "INFO",
		"msg":        "iniciado",
		"service":    "wager-wallet-service",
		"instanceId": "local-1",
		"walletId":   "w-1",
	}
	for k, v := range want {
		if rec[k] != v {
			t.Errorf("%s = %v, want %q", k, rec[k], v)
		}
	}
}

func TestNewRespectsLevel(t *testing.T) {
	var buf bytes.Buffer
	log := New(&buf, slog.LevelWarn, "local-1")

	log.Info("não deve aparecer")

	if buf.Len() != 0 {
		t.Errorf("registro abaixo do nível foi escrito: %q", buf.String())
	}
}
