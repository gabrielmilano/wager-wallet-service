package wagering

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/gabrielmilano/wager-wallet-service/internal/app/apperr"
	"github.com/gabrielmilano/wager-wallet-service/internal/domain/money"
	"github.com/gabrielmilano/wager-wallet-service/internal/domain/wager"
)

// maxIDLength limita identificadores textuais (provedor, ids externos,
// rodada, jogo, chave): evita entradas gigantes nos índices únicos.
const maxIDLength = 255

// Input são os campos de negócio como chegam do HTTP ou do SQS (o Money já
// passou pelo UnmarshalJSON estrito).
type Input struct {
	ProviderID                     string
	ExternalTransactionID          string
	PlayerID                       string
	WalletID                       string
	RoundID                        string
	GameID                         string
	Kind                           string
	Money                          money.Money
	ReferenceExternalTransactionID string
}

// Command é uma operação validada. Contém só campos de negócio: é exatamente
// o que entra no hash de idempotência (ADR 0006). Metadados de transporte
// (chave de idempotência, correlationId, messageId) ficam em Metadata.
type Command struct {
	ProviderID                     string
	ExternalTransactionID          string
	PlayerID                       uuid.UUID
	WalletID                       uuid.UUID
	RoundID                        string
	GameID                         string
	Kind                           wager.Kind
	Money                          money.Money
	ReferenceExternalTransactionID string
}

// Metadata são os dados de transporte da requisição ou mensagem.
type Metadata struct {
	IdempotencyKey string
	CorrelationID  string
	CausationID    string // opcional (no SQS, o messageId)
}

// NewCommand valida a entrada externa. Todos os problemas viram um único
// VALIDATION_ERROR (corrigível, nada é gravado). É aqui que fica a regra
// "valor não negativo para entrada externa" (ADR 0004).
func NewCommand(in Input) (Command, error) {
	var problems []string
	fail := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	requireID := func(name, value string) {
		switch {
		case value == "":
			fail("%s é obrigatório", name)
		case len(value) > maxIDLength:
			fail("%s excede %d caracteres", name, maxIDLength)
		}
	}
	requireID("providerId", in.ProviderID)
	requireID("externalTransactionId", in.ExternalTransactionID)
	requireID("roundId", in.RoundID)
	requireID("gameId", in.GameID)

	playerID := parseUUID("playerId", in.PlayerID, fail)
	walletID := parseUUID("walletId", in.WalletID, fail)

	kind, err := wager.ParseExternalKind(in.Kind)
	if err != nil {
		fail("kind inválido: use BET, WIN, LOSS, REFUND ou ROLLBACK")
	}

	switch {
	case !in.Money.Valid():
		fail("money é obrigatório")
	case in.Money.IsNegative():
		fail("money.amount não pode ser negativo")
	case kind != "":
		if err := wager.ValidateAmount(kind, in.Money); err != nil {
			fail("%s", strings.TrimPrefix(err.Error(), wager.ErrInvalidTransaction.Error()+": "))
		}
	}

	if kind != "" {
		ref := in.ReferenceExternalTransactionID
		switch {
		case kind.RequiresReference() && ref == "":
			fail("%s exige referenceExternalTransactionId", kind)
		case !kind.AcceptsReference() && ref != "":
			fail("%s não aceita referenceExternalTransactionId", kind)
		case len(ref) > maxIDLength:
			fail("referenceExternalTransactionId excede %d caracteres", maxIDLength)
		}
	}

	if len(problems) > 0 {
		return Command{}, apperr.ValidationError(strings.Join(problems, "; "), nil)
	}
	return Command{
		ProviderID:                     in.ProviderID,
		ExternalTransactionID:          in.ExternalTransactionID,
		PlayerID:                       playerID,
		WalletID:                       walletID,
		RoundID:                        in.RoundID,
		GameID:                         in.GameID,
		Kind:                           kind,
		Money:                          in.Money,
		ReferenceExternalTransactionID: in.ReferenceExternalTransactionID,
	}, nil
}

// parseUUID aceita só a forma canônica de 36 caracteres (maiúsculas ou
// minúsculas); a forma minúscula é a usada no hash.
func parseUUID(name, value string, fail func(string, ...any)) uuid.UUID {
	if len(value) != 36 {
		fail("%s deve ser um UUID (xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx)", name)
		return uuid.Nil
	}
	id, err := uuid.Parse(value)
	if err != nil || id == uuid.Nil {
		fail("%s deve ser um UUID válido", name)
		return uuid.Nil
	}
	return id
}

// ValidateMetadata exige a chave de idempotência (header Idempotency-Key ou
// data.idempotencyKey). O servidor nunca a substitui por outra calculada.
func ValidateMetadata(m Metadata) error {
	switch {
	case m.IdempotencyKey == "":
		return apperr.ValidationError("Idempotency-Key é obrigatória", nil)
	case len(m.IdempotencyKey) > maxIDLength:
		return apperr.ValidationError(fmt.Sprintf("Idempotency-Key excede %d caracteres", maxIDLength), nil)
	}
	return nil
}

// CanonicalJSON é a representação usada no hash de idempotência:
//
//   - só campos de negócio (sem chave de idempotência nem metadados de
//     transporte), com as chaves em ordem alfabética (encoding/json ordena as
//     chaves de mapas);
//   - money como {"amount":"25.00","currency":"BRL"}, no formato canônico
//     (o parsing não aceita formas equivalentes, então não há normalização);
//   - UUIDs na forma canônica minúscula;
//   - referenceExternalTransactionId omitido quando ausente.
//
// HTTP e SQS montam o mesmo Command, então produzem o mesmo hash.
func (c Command) CanonicalJSON() []byte {
	fields := map[string]any{
		"providerId":            c.ProviderID,
		"externalTransactionId": c.ExternalTransactionID,
		"playerId":              c.PlayerID.String(),
		"walletId":              c.WalletID.String(),
		"roundId":               c.RoundID,
		"gameId":                c.GameID,
		"kind":                  string(c.Kind),
		"money": map[string]string{
			"amount":   c.Money.Amount(),
			"currency": string(c.Money.Currency()),
		},
	}
	if c.ReferenceExternalTransactionID != "" {
		fields["referenceExternalTransactionId"] = c.ReferenceExternalTransactionID
	}
	data, err := json.Marshal(fields)
	if err != nil {
		// Só strings e mapas de strings: Marshal não falha.
		panic(errors.Join(errors.New("wagering: JSON canônico"), err))
	}
	return data
}

// Hash é o SHA-256 do JSON canônico.
func (c Command) Hash() []byte {
	sum := sha256.Sum256(c.CanonicalJSON())
	return sum[:]
}
