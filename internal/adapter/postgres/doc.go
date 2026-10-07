// Package postgres implementa os repositórios e o TxRunner com pgx e SQL
// explícito: abertura da transação, lock_timeout, SELECT ... FOR UPDATE e
// commit ou rollback (ADR 0005).
//
// Implementação na Fase 05.
package postgres
