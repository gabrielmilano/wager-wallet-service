// Package app agrupa os casos de uso do serviço.
//
// Os casos de uso orquestram o domínio e dependem apenas de interfaces
// (portas) declaradas aqui, como store.TxRunner. As implementações concretas
// ficam em internal/adapter e são ligadas pelo Fx em internal/bootstrap.
//
// Regra de dependência: app importa domain e platform, nunca adapter,
// bootstrap, Fx, net/http, pgx ou o SDK da AWS.
package app
