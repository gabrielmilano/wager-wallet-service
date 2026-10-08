package money

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// wire é o formato externo: {"amount":"25.00","currency":"BRL"} (ADR 0004).
type wire struct {
	Amount   string   `json:"amount"`
	Currency Currency `json:"currency"`
}

// MarshalJSON produz sempre o formato canônico, com sinal quando negativo
// (ex.: a diferença da reconciliação).
func (m Money) MarshalJSON() ([]byte, error) {
	if !m.Valid() {
		return nil, ErrUninitialized
	}
	return json.Marshal(wire{Amount: m.Amount(), Currency: m.currency})
}

// UnmarshalJSON é estrito: objeto com exatamente "amount" e "currency",
// ambos strings JSON. Número JSON (25.00 sem aspas) é recusado, então o valor
// nunca passa por um decodificador de ponto flutuante. Aceita sinal; a regra
// de "não negativo" para entradas externas fica no Command (ADR 0004).
//
// Como todo Unmarshaler, altera o próprio receptor: é o único ponto em que um
// Money muda depois de criado, e só durante a decodificação.
func (m *Money) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return fmt.Errorf("%w: money deve ser um objeto {\"amount\",\"currency\"}", ErrInvalidAmount)
	}
	for k := range fields {
		if k != "amount" && k != "currency" {
			return fmt.Errorf("%w: campo desconhecido %q em money", ErrInvalidAmount, k)
		}
	}

	amount, err := requiredString(fields, "amount")
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidAmount, err)
	}
	currency, err := requiredString(fields, "currency")
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidCurrency, err)
	}

	parsed, err := Parse(amount, currency)
	if err != nil {
		return err
	}
	*m = parsed
	return nil
}

// requiredString exige que o campo exista e seja uma string JSON.
func requiredString(fields map[string]json.RawMessage, name string) (string, error) {
	raw, ok := fields[name]
	if !ok {
		return "", fmt.Errorf("campo %q obrigatório", name)
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '"' {
		return "", fmt.Errorf("campo %q deve ser string JSON (ex.: \"25.00\")", name)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", fmt.Errorf("campo %q: %v", name, err)
	}
	return s, nil
}
