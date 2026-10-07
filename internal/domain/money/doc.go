// Package money define Money, value object imutável com valor em unidades
// mínimas (int64) e moeda ISO 4217, com parsing a partir de string decimal,
// aritmética com verificação de overflow e serialização JSON no formato
// {"amount":"25.00","currency":"BRL"} (ADR 0004).
//
// Implementação na Fase 04.
package money
