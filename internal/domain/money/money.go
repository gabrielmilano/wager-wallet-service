package money

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Erros do pacote, classificáveis com errors.Is.
var (
	ErrInvalidAmount    = errors.New("money: valor inválido")
	ErrInvalidCurrency  = errors.New("money: moeda inválida")
	ErrOverflow         = errors.New("money: estouro de int64")
	ErrCurrencyMismatch = errors.New("money: moedas diferentes")
	ErrUninitialized    = errors.New("money: valor não inicializado")
)

// Currency é um código ISO 4217 da lista fechada aceita pelo serviço. A mesma
// lista está no CHECK wallets_currency_supported do banco.
type Currency string

const (
	BRL Currency = "BRL"
	USD Currency = "USD"
	EUR Currency = "EUR"
)

// ParseCurrency aceita apenas os códigos suportados, em maiúsculas.
func ParseCurrency(s string) (Currency, error) {
	switch c := Currency(s); c {
	case BRL, USD, EUR:
		return c, nil
	default:
		return "", fmt.Errorf("%w: %q (aceitas: BRL, USD, EUR)", ErrInvalidCurrency, s)
	}
}

// scale é o número fixo de casas decimais (centavos).
const scale = 2

// Money é um value object imutável: valor em unidades mínimas (centavos) e
// moeda. O zero value (Money{}) não é válido: não tem moeda.
//
// Limites: de -92.233.720.368.547.758,08 a 92.233.720.368.547.758,07
// (int64 em centavos). Operações que passariam disso devolvem ErrOverflow.
type Money struct {
	minor    int64
	currency Currency
}

// New cria um Money a partir de unidades mínimas.
func New(minor int64, currency Currency) (Money, error) {
	if _, err := ParseCurrency(string(currency)); err != nil {
		return Money{}, err
	}
	return Money{minor: minor, currency: currency}, nil
}

// Zero devolve o valor zero da moeda.
func Zero(currency Currency) (Money, error) {
	return New(0, currency)
}

// Parse lê um valor decimal em formato canônico: sinal "-" opcional, parte
// inteira sem zeros à esquerda e exatamente duas casas decimais
// ("25.00", "0.50", "-5.00"). Recusa vazio, espaços, "+", NaN, Infinity,
// notação científica, vírgula, escala diferente de 2 e "-0.00". Não há
// arredondamento nem normalização: entrada fora do formato é erro.
func Parse(amount string, currency string) (Money, error) {
	c, err := ParseCurrency(currency)
	if err != nil {
		return Money{}, err
	}
	minor, err := parseMinor(amount)
	if err != nil {
		return Money{}, err
	}
	return Money{minor: minor, currency: c}, nil
}

func parseMinor(s string) (int64, error) {
	invalid := func(reason string) error {
		return fmt.Errorf("%w: %q (%s)", ErrInvalidAmount, s, reason)
	}

	digits := s
	negative := false
	if rest, ok := strings.CutPrefix(s, "-"); ok {
		negative = true
		digits = rest
	}

	intPart, fracPart, ok := strings.Cut(digits, ".")
	if !ok {
		return 0, invalid("use o formato 0.00")
	}
	if len(fracPart) != scale {
		return 0, invalid("exatamente duas casas decimais")
	}
	if intPart == "" || !allDigits(intPart) || !allDigits(fracPart) {
		return 0, invalid("apenas dígitos, sinal - opcional e ponto")
	}
	if len(intPart) > 1 && intPart[0] == '0' {
		return 0, invalid("sem zeros à esquerda")
	}
	if negative && intPart == "0" && fracPart == "00" {
		return 0, invalid("-0.00 não é canônico; use 0.00")
	}

	// "25.00" -> "2500": os dígitos sem o ponto já são as unidades mínimas.
	// ParseInt detecta o estouro de int64 (strconv.ErrRange).
	unsigned := intPart + fracPart
	if negative {
		unsigned = "-" + unsigned
	}
	minor, err := strconv.ParseInt(unsigned, 10, 64)
	if errors.Is(err, strconv.ErrRange) {
		return 0, fmt.Errorf("%w: %q", ErrOverflow, s)
	}
	if err != nil {
		return 0, invalid(err.Error())
	}
	return minor, nil
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// Minor devolve o valor em unidades mínimas (centavos).
func (m Money) Minor() int64 { return m.minor }

// Currency devolve a moeda.
func (m Money) Currency() Currency { return m.currency }

// Valid informa se o valor foi inicializado por um construtor.
func (m Money) Valid() bool { return m.currency != "" }

func (m Money) IsZero() bool     { return m.minor == 0 }
func (m Money) IsPositive() bool { return m.minor > 0 }
func (m Money) IsNegative() bool { return m.minor < 0 }

// Amount devolve o valor no formato canônico, sem moeda: "25.00", "-5.00".
func (m Money) Amount() string {
	sign := ""
	// Trabalha com uint64 para que math.MinInt64 também seja formatado.
	u := uint64(m.minor)
	if m.minor < 0 {
		sign = "-"
		u = uint64(-(m.minor + 1)) + 1
	}
	return fmt.Sprintf("%s%d.%02d", sign, u/100, u%100)
}

// String devolve valor e moeda: "25.00 BRL".
func (m Money) String() string {
	if !m.Valid() {
		return "<money não inicializado>"
	}
	return m.Amount() + " " + string(m.currency)
}

// sameCurrency valida que os dois valores foram inicializados e têm a mesma
// moeda: pré-condição de toda aritmética e comparação.
func (m Money) sameCurrency(o Money) error {
	if !m.Valid() || !o.Valid() {
		return ErrUninitialized
	}
	if m.currency != o.currency {
		return fmt.Errorf("%w: %s e %s", ErrCurrencyMismatch, m.currency, o.currency)
	}
	return nil
}

// Add soma dois valores da mesma moeda.
func (m Money) Add(o Money) (Money, error) {
	if err := m.sameCurrency(o); err != nil {
		return Money{}, err
	}
	if (o.minor > 0 && m.minor > math.MaxInt64-o.minor) ||
		(o.minor < 0 && m.minor < math.MinInt64-o.minor) {
		return Money{}, fmt.Errorf("%w: %s + %s", ErrOverflow, m, o)
	}
	return Money{minor: m.minor + o.minor, currency: m.currency}, nil
}

// Sub subtrai o de m (mesma moeda).
func (m Money) Sub(o Money) (Money, error) {
	if err := m.sameCurrency(o); err != nil {
		return Money{}, err
	}
	if (o.minor < 0 && m.minor > math.MaxInt64+o.minor) ||
		(o.minor > 0 && m.minor < math.MinInt64+o.minor) {
		return Money{}, fmt.Errorf("%w: %s - %s", ErrOverflow, m, o)
	}
	return Money{minor: m.minor - o.minor, currency: m.currency}, nil
}

// Neg devolve o oposto. -MinInt64 não cabe em int64.
func (m Money) Neg() (Money, error) {
	if !m.Valid() {
		return Money{}, ErrUninitialized
	}
	if m.minor == math.MinInt64 {
		return Money{}, fmt.Errorf("%w: -(%s)", ErrOverflow, m)
	}
	return Money{minor: -m.minor, currency: m.currency}, nil
}

// Compare devolve -1, 0 ou 1 (m < o, m == o, m > o). Exige a mesma moeda.
func (m Money) Compare(o Money) (int, error) {
	if err := m.sameCurrency(o); err != nil {
		return 0, err
	}
	switch {
	case m.minor < o.minor:
		return -1, nil
	case m.minor > o.minor:
		return 1, nil
	default:
		return 0, nil
	}
}

// Equal informa se valor e moeda são iguais. Valores de moedas diferentes
// (ou não inicializados) não são iguais.
func (m Money) Equal(o Money) bool {
	return m.sameCurrency(o) == nil && m.minor == o.minor
}
