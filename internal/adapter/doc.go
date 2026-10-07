// Package adapter agrupa as implementações concretas das portas declaradas em
// internal/app e as entradas do serviço (HTTP e SQS).
//
// Regra de dependência: adapter importa app, domain e platform, nunca
// bootstrap. É a única camada que conhece pgx, net/http, o SDK da AWS e a
// biblioteca de OIDC.
package adapter
