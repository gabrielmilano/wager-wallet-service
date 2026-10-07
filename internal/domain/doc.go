// Package domain agrupa o modelo de domínio do serviço: Money, Wallet,
// WagerTransaction, LedgerEntry e os eventos de integração.
//
// Regra de dependência: os subpacotes de domain importam apenas a biblioteca
// padrão, o pacote de UUID e outros subpacotes de domain. Nunca importam app,
// adapter, platform, bootstrap, Fx, net/http, pgx ou o SDK da AWS. O teste em
// internal/archtest garante essa regra.
package domain
