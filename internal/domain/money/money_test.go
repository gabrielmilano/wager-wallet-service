package money

import (
	"encoding/json"
	"errors"
	"math"
	"testing"
)

func mustParse(t *testing.T, amount string, c Currency) Money {
	t.Helper()
	m, err := Parse(amount, string(c))
	if err != nil {
		t.Fatalf("Parse(%q): %v", amount, err)
	}
	return m
}

func TestParseValid(t *testing.T) {
	tests := []struct {
		in    string
		minor int64
	}{
		{"0.00", 0},
		{"0.01", 1},
		{"0.50", 50},
		{"25.00", 2500},
		{"1000.00", 100000},
		{"-5.00", -500},
		{"-0.01", -1},
		{"92233720368547758.07", math.MaxInt64},
		{"-92233720368547758.08", math.MinInt64},
	}
	for _, tt := range tests {
		m, err := Parse(tt.in, "BRL")
		if err != nil {
			t.Errorf("Parse(%q): %v", tt.in, err)
			continue
		}
		if m.Minor() != tt.minor || m.Currency() != BRL {
			t.Errorf("Parse(%q) = %d %s, want %d BRL", tt.in, m.Minor(), m.Currency(), tt.minor)
		}
		if got := m.Amount(); got != tt.in {
			t.Errorf("Parse(%q).Amount() = %q (ida e volta)", tt.in, got)
		}
	}
}

func TestParseRejectsInvalidAmounts(t *testing.T) {
	invalid := []string{
		"", " ", "25", "25.0", "25.000", "25,00", "+25.00", " 25.00", "25.00 ",
		"025.00", "00.00", "-0.00", ".50", "25.", "-", "-.50", "1e3", "1.5e2", "2.5E1",
		"NaN", "nan", "Infinity", "-Infinity", "Inf", "0x10.00", "25.0a", "1_000.00",
		"--5.00", "5.-0",
	}
	for _, in := range invalid {
		if _, err := Parse(in, "BRL"); !errors.Is(err, ErrInvalidAmount) {
			t.Errorf("Parse(%q) erro = %v, want ErrInvalidAmount", in, err)
		}
	}
}

func TestParseOverflow(t *testing.T) {
	for _, in := range []string{"92233720368547758.08", "-92233720368547758.09", "99999999999999999999.00"} {
		if _, err := Parse(in, "BRL"); !errors.Is(err, ErrOverflow) {
			t.Errorf("Parse(%q) erro = %v, want ErrOverflow", in, err)
		}
	}
}

func TestParseCurrency(t *testing.T) {
	for _, c := range []string{"BRL", "USD", "EUR"} {
		if _, err := ParseCurrency(c); err != nil {
			t.Errorf("ParseCurrency(%q): %v", c, err)
		}
	}
	for _, c := range []string{"", "brl", "GBP", "BR", "BRLL", " BRL"} {
		if _, err := ParseCurrency(c); !errors.Is(err, ErrInvalidCurrency) {
			t.Errorf("ParseCurrency(%q) erro = %v, want ErrInvalidCurrency", c, err)
		}
		if _, err := Parse("1.00", c); !errors.Is(err, ErrInvalidCurrency) {
			t.Errorf("Parse com moeda %q erro = %v, want ErrInvalidCurrency", c, err)
		}
	}
}

func TestZeroAndNew(t *testing.T) {
	z, err := Zero(USD)
	if err != nil || !z.IsZero() || z.Currency() != USD || z.Amount() != "0.00" {
		t.Errorf("Zero(USD) = %v, %v", z, err)
	}
	if _, err := New(1, "XYZ"); !errors.Is(err, ErrInvalidCurrency) {
		t.Errorf("New com moeda inválida: %v", err)
	}
	if (Money{}).Valid() {
		t.Error("Money{} não deveria ser válido")
	}
}

func TestArithmetic(t *testing.T) {
	a := mustParse(t, "10.00", BRL)
	b := mustParse(t, "2.50", BRL)

	sum, err := a.Add(b)
	if err != nil || sum.Amount() != "12.50" {
		t.Errorf("10.00 + 2.50 = %v, %v", sum, err)
	}
	diff, err := b.Sub(a)
	if err != nil || diff.Amount() != "-7.50" || !diff.IsNegative() {
		t.Errorf("2.50 - 10.00 = %v, %v", diff, err)
	}
	neg, err := a.Neg()
	if err != nil || neg.Amount() != "-10.00" {
		t.Errorf("-(10.00) = %v, %v", neg, err)
	}
	if c, err := a.Compare(b); err != nil || c != 1 {
		t.Errorf("Compare = %d, %v", c, err)
	}
	if c, _ := b.Compare(a); c != -1 {
		t.Errorf("Compare invertido = %d", c)
	}
	if c, _ := a.Compare(mustParse(t, "10.00", BRL)); c != 0 {
		t.Errorf("Compare igual = %d", c)
	}
	if !a.Equal(mustParse(t, "10.00", BRL)) || a.Equal(b) {
		t.Error("Equal incorreto")
	}
}

func TestArithmeticOverflow(t *testing.T) {
	max, _ := New(math.MaxInt64, BRL)
	min, _ := New(math.MinInt64, BRL)
	one, _ := New(1, BRL)

	if _, err := max.Add(one); !errors.Is(err, ErrOverflow) {
		t.Errorf("max + 1: %v", err)
	}
	if _, err := min.Sub(one); !errors.Is(err, ErrOverflow) {
		t.Errorf("min - 1: %v", err)
	}
	if _, err := min.Add(min); !errors.Is(err, ErrOverflow) {
		t.Errorf("min + min: %v", err)
	}
	minusOne, _ := one.Neg()
	if _, err := max.Sub(minusOne); !errors.Is(err, ErrOverflow) {
		t.Errorf("max - (-1): %v", err)
	}
	if _, err := min.Neg(); !errors.Is(err, ErrOverflow) {
		t.Errorf("-(min): %v", err)
	}
	// Limites exatos não estouram.
	if r, err := max.Sub(one); err != nil || r.Minor() != math.MaxInt64-1 {
		t.Errorf("max - 1 = %v, %v", r, err)
	}
	if min.Amount() != "-92233720368547758.08" {
		t.Errorf("min.Amount() = %q", min.Amount())
	}
}

func TestCurrencyMismatch(t *testing.T) {
	brl := mustParse(t, "1.00", BRL)
	usd := mustParse(t, "1.00", USD)

	if _, err := brl.Add(usd); !errors.Is(err, ErrCurrencyMismatch) {
		t.Errorf("Add: %v", err)
	}
	if _, err := brl.Sub(usd); !errors.Is(err, ErrCurrencyMismatch) {
		t.Errorf("Sub: %v", err)
	}
	if _, err := brl.Compare(usd); !errors.Is(err, ErrCurrencyMismatch) {
		t.Errorf("Compare: %v", err)
	}
	if brl.Equal(usd) {
		t.Error("Equal entre moedas diferentes")
	}
}

func TestUninitializedIsRejected(t *testing.T) {
	var zero Money
	one := mustParse(t, "1.00", BRL)
	if _, err := zero.Add(one); !errors.Is(err, ErrUninitialized) {
		t.Errorf("Add: %v", err)
	}
	if _, err := one.Sub(zero); !errors.Is(err, ErrUninitialized) {
		t.Errorf("Sub: %v", err)
	}
	if _, err := zero.Neg(); !errors.Is(err, ErrUninitialized) {
		t.Errorf("Neg: %v", err)
	}
	if _, err := json.Marshal(zero); !errors.Is(err, ErrUninitialized) {
		t.Errorf("Marshal: %v", err)
	}
}

func TestJSONRoundTrip(t *testing.T) {
	for _, in := range []string{"25.00", "0.00", "-5.00", "92233720368547758.07"} {
		m := mustParse(t, in, EUR)
		data, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		want := `{"amount":"` + in + `","currency":"EUR"}`
		if string(data) != want {
			t.Errorf("Marshal = %s, want %s", data, want)
		}
		var back Money
		if err := json.Unmarshal(data, &back); err != nil || !back.Equal(m) {
			t.Errorf("Unmarshal(%s) = %v, %v", data, back, err)
		}
	}
}

func TestJSONUnmarshalIsStrict(t *testing.T) {
	tests := []struct {
		in      string
		wantErr error
	}{
		{`{"amount":25.00,"currency":"BRL"}`, ErrInvalidAmount},    // número JSON
		{`{"amount":2500,"currency":"BRL"}`, ErrInvalidAmount},     // inteiro JSON
		{`{"amount":null,"currency":"BRL"}`, ErrInvalidAmount},     // null
		{`{"currency":"BRL"}`, ErrInvalidAmount},                   // sem amount
		{`{"amount":"25.00"}`, ErrInvalidCurrency},                 // sem currency
		{`{"amount":"25.00","currency":null}`, ErrInvalidCurrency}, // currency null
		{`{"amount":"25.00","currency":"brl"}`, ErrInvalidCurrency},
		{`{"amount":"25","currency":"BRL"}`, ErrInvalidAmount},
		{`{"amount":"1e3","currency":"BRL"}`, ErrInvalidAmount},
		{`{"amount":"NaN","currency":"BRL"}`, ErrInvalidAmount},
		{`{"amount":"25.00","currency":"BRL","extra":1}`, ErrInvalidAmount},
		{`"25.00"`, ErrInvalidAmount},
		{`null`, ErrInvalidAmount},
		{`[]`, ErrInvalidAmount},
	}
	for _, tt := range tests {
		var m Money
		if err := json.Unmarshal([]byte(tt.in), &m); !errors.Is(err, tt.wantErr) {
			t.Errorf("Unmarshal(%s) erro = %v, want %v", tt.in, err, tt.wantErr)
		}
	}
}

func TestJSONUnmarshalAcceptsSign(t *testing.T) {
	var m Money
	if err := json.Unmarshal([]byte(`{"amount":"-5.00","currency":"BRL"}`), &m); err != nil {
		t.Fatal(err)
	}
	if m.Minor() != -500 {
		t.Errorf("minor = %d, want -500", m.Minor())
	}
}

func TestJSONInsideStruct(t *testing.T) {
	var req struct {
		Money Money `json:"money"`
	}
	if err := json.Unmarshal([]byte(`{"money":{"amount":"25.00","currency":"BRL"}}`), &req); err != nil {
		t.Fatal(err)
	}
	if req.Money.Minor() != 2500 {
		t.Errorf("minor = %d", req.Money.Minor())
	}
	if err := json.Unmarshal([]byte(`{"money":{"amount":25.00,"currency":"BRL"}}`), &req); err == nil {
		t.Error("número JSON dentro de struct deveria falhar")
	}
}
