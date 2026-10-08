package wagering

import (
	"context"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gabrielmilano/wager-wallet-service/internal/app/apperr"
	"github.com/gabrielmilano/wager-wallet-service/internal/app/store"
	"github.com/gabrielmilano/wager-wallet-service/internal/domain/money"
	"github.com/gabrielmilano/wager-wallet-service/internal/domain/wager"
)

func brl(t *testing.T, s string) money.Money {
	t.Helper()
	m, err := money.Parse(s, "BRL")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func validInput(t *testing.T) Input {
	return Input{
		ProviderID:            "provider-a",
		ExternalTransactionID: "transaction-123",
		PlayerID:              "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1",
		WalletID:              "0192f291-27dd-7d3f-8071-5f8685deef37",
		RoundID:               "round-987",
		GameID:                "fortune-chimp",
		Kind:                  "BET",
		Money:                 brl(t, "25.00"),
	}
}

func TestNewCommandValid(t *testing.T) {
	cmd, err := NewCommand(validInput(t))
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Kind != wager.Bet || cmd.Money.Minor() != 2500 || cmd.PlayerID.String() != "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1" {
		t.Errorf("command = %+v", cmd)
	}
}

func TestNewCommandRejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name   string
		change func(in *Input)
		want   string
	}{
		{"sem provedor", func(in *Input) { in.ProviderID = "" }, "providerId"},
		{"sem id externo", func(in *Input) { in.ExternalTransactionID = "" }, "externalTransactionId"},
		{"id gigante", func(in *Input) { in.RoundID = strings.Repeat("x", 256) }, "roundId excede"},
		{"jogador não UUID", func(in *Input) { in.PlayerID = "player-1" }, "playerId"},
		{"UUID sem hífens", func(in *Input) { in.WalletID = "0192f29127dd7d3f80715f8685deef37" }, "walletId"},
		{"tipo OPENING", func(in *Input) { in.Kind = "OPENING" }, "kind inválido"},
		{"tipo desconhecido", func(in *Input) { in.Kind = "CASHOUT" }, "kind inválido"},
		{"money ausente", func(in *Input) { in.Money = money.Money{} }, "money é obrigatório"},
		{"valor negativo", func(in *Input) { in.Money = brl(t, "-1.00") }, "negativo"},
		{"BET zero", func(in *Input) { in.Money = brl(t, "0.00") }, "BET exige amount > 0"},
		{"LOSS com valor", func(in *Input) { in.Kind = "LOSS" }, "LOSS exige amount 0.00"},
		{"REFUND sem referência", func(in *Input) { in.Kind = "REFUND" }, "REFUND exige referenceExternalTransactionId"},
		{"BET com referência", func(in *Input) { in.ReferenceExternalTransactionID = "x" }, "BET não aceita"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := validInput(t)
			tt.change(&in)
			_, err := NewCommand(in)
			e, ok := apperr.As(err)
			if !ok || e.Code != apperr.CodeValidation || !strings.Contains(e.Message, tt.want) {
				t.Errorf("erro = %v, want VALIDATION_ERROR contendo %q", err, tt.want)
			}
		})
	}
}

func TestNewCommandReportsAllProblems(t *testing.T) {
	_, err := NewCommand(Input{Kind: "BET"})
	e, _ := apperr.As(err)
	for _, want := range []string{"providerId", "externalTransactionId", "playerId", "walletId", "roundId", "gameId", "money"} {
		if e == nil || !strings.Contains(e.Message, want) {
			t.Errorf("mensagem não cita %s: %v", want, err)
		}
	}
}

// O JSON canônico é o contrato do hash: chaves em ordem alfabética, money
// como strings, sem chave de idempotência. Mudá-lo invalida os hashes já
// gravados, por isso o valor é fixo no teste.
func TestCanonicalJSONIsStable(t *testing.T) {
	cmd, _ := NewCommand(validInput(t))
	want := `{"externalTransactionId":"transaction-123","gameId":"fortune-chimp","kind":"BET",` +
		`"money":{"amount":"25.00","currency":"BRL"},"playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1",` +
		`"providerId":"provider-a","roundId":"round-987","walletId":"0192f291-27dd-7d3f-8071-5f8685deef37"}`
	if got := string(cmd.CanonicalJSON()); got != want {
		t.Errorf("JSON canônico =\n%s\nwant\n%s", got, want)
	}
	if got := hex.EncodeToString(cmd.Hash()); len(got) != 64 {
		t.Errorf("hash = %s, want SHA-256", got)
	}
}

func TestHashEquivalenceAndSensitivity(t *testing.T) {
	base, _ := NewCommand(validInput(t))

	// Equivalente: UUID em maiúsculas vira a forma canônica minúscula.
	upper := validInput(t)
	upper.PlayerID = strings.ToUpper(upper.PlayerID)
	same, _ := NewCommand(upper)
	if hex.EncodeToString(same.Hash()) != hex.EncodeToString(base.Hash()) {
		t.Error("UUID em maiúsculas deveria gerar o mesmo hash")
	}

	// Qualquer campo de negócio muda o hash.
	for name, change := range map[string]func(in *Input){
		"valor":  func(in *Input) { in.Money = brl(t, "25.01") },
		"rodada": func(in *Input) { in.RoundID = "round-988" },
		"jogo":   func(in *Input) { in.GameID = "outro" },
		"tipo":   func(in *Input) { in.Kind = "WIN" },
		"moeda": func(in *Input) {
			in.Money, _ = money.Parse("25.00", "USD")
		},
	} {
		in := validInput(t)
		change(&in)
		other, err := NewCommand(in)
		if err != nil {
			t.Fatal(err)
		}
		if hex.EncodeToString(other.Hash()) == hex.EncodeToString(base.Hash()) {
			t.Errorf("mudar %s não mudou o hash", name)
		}
	}
}

func TestValidateMetadata(t *testing.T) {
	if err := ValidateMetadata(Metadata{}); !apperr.IsKind(err, apperr.Validation) {
		t.Errorf("sem chave: %v", err)
	}
	if err := ValidateMetadata(Metadata{IdempotencyKey: strings.Repeat("k", 256)}); !apperr.IsKind(err, apperr.Validation) {
		t.Errorf("chave gigante: %v", err)
	}
	if err := ValidateMetadata(Metadata{IdempotencyKey: "provider-a:transaction-123"}); err != nil {
		t.Errorf("chave válida: %v", err)
	}
}

// --- lookup: corrida entre as duas buscas ------------------------------------

// fakeTransactions implementa só as buscas usadas pelo lookup; os demais
// métodos vêm da interface embutida (nil) e não são chamados.
type fakeTransactions struct {
	store.TransactionRepository
	byKey, byExternal *wager.Transaction
}

func (f fakeTransactions) FindByIdempotencyKey(context.Context, string, string) (*wager.Transaction, error) {
	if f.byKey == nil {
		return nil, store.ErrNotFound
	}
	return f.byKey, nil
}

func (f fakeTransactions) FindByExternalID(context.Context, string, string) (*wager.Transaction, error) {
	if f.byExternal == nil {
		return nil, store.ErrNotFound
	}
	return f.byExternal, nil
}

func persisted(t *testing.T, cmd Command, key string) *wager.Transaction {
	t.Helper()
	tx, err := wager.NewExternal(wager.ExternalParams{
		ID: uuid.New(), WalletID: cmd.WalletID, PlayerID: cmd.PlayerID, ProviderID: cmd.ProviderID,
		ExternalID: cmd.ExternalTransactionID, IdempotencyKey: key, PayloadHash: cmd.Hash(),
		RoundID: cmd.RoundID, GameID: cmd.GameID, Kind: cmd.Kind, Money: cmd.Money, Now: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return tx
}

func TestLookup(t *testing.T) {
	ctx := context.Background()
	cmd, _ := NewCommand(validInput(t))
	key := "provider-a:transaction-123"
	mine := persisted(t, cmd, key)

	t.Run("nada encontrado", func(t *testing.T) {
		_, found, err := lookup(ctx, store.Repos{Transactions: fakeTransactions{}}, cmd, key, cmd.Hash())
		if found || err != nil {
			t.Errorf("found=%v err=%v", found, err)
		}
	})

	t.Run("mesma chave e conteúdo: replay", func(t *testing.T) {
		res, found, err := lookup(ctx, store.Repos{Transactions: fakeTransactions{byKey: mine}}, cmd, key, cmd.Hash())
		if !found || err != nil || !res.Replay || res.Transaction != mine {
			t.Errorf("res=%+v found=%v err=%v", res, found, err)
		}
	})

	t.Run("mesma chave, outro conteúdo: conflito", func(t *testing.T) {
		_, _, err := lookup(ctx, store.Repos{Transactions: fakeTransactions{byKey: mine}}, cmd, key, []byte{1})
		if e, ok := apperr.As(err); !ok || e.Code != apperr.CodeIdempotencyConflict {
			t.Errorf("err=%v", err)
		}
	})

	t.Run("mesmo id externo com outra chave: conflito", func(t *testing.T) {
		other := persisted(t, cmd, "outra-chave")
		_, _, err := lookup(ctx, store.Repos{Transactions: fakeTransactions{byExternal: other}}, cmd, key, cmd.Hash())
		if e, ok := apperr.As(err); !ok || e.Code != apperr.CodeIdempotencyConflict {
			t.Errorf("err=%v", err)
		}
	})

	// Regressão: a primeira busca (pela chave) roda antes do commit da
	// requisição concorrente e não acha nada; a segunda (pelo id externo) roda
	// depois e acha a operação com a MESMA chave. É replay, não conflito.
	t.Run("commit concorrente entre as duas buscas: replay", func(t *testing.T) {
		res, found, err := lookup(ctx, store.Repos{Transactions: fakeTransactions{byExternal: mine}}, cmd, key, cmd.Hash())
		if !found || err != nil || !res.Replay {
			t.Errorf("res=%+v found=%v err=%v", res, found, err)
		}
	})
}
