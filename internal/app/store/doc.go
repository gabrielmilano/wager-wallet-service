// Package store declara as portas de persistência usadas pelos casos de uso:
// os repositórios e o TxRunner, que delimita a transação SQL entre eles
// (ADR 0005). A implementação fica em internal/adapter/postgres.
//
// Implementação na Fase 05.
package store
