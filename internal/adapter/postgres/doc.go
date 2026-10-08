// Package postgres implementa os repositórios e o TxRunner com pgx e SQL
// explícito: abertura da transação, lock_timeout, SELECT ... FOR UPDATE e
// commit ou rollback (ADR 0005).
//
// Na Fase 03 contém o Migrator (ADR 0010); repositórios e TxRunner entram na
// Fase 05.
package postgres
