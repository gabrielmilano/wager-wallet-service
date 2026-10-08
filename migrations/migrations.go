// Package migrations embute no binário os arquivos SQL de evolução do banco
// (golang-migrate: NNNNNN_nome.up.sql e NNNNNN_nome.down.sql). Eles são
// aplicados pelo subcomando "wallet-service migrate" (ADR 0010).
package migrations

import "embed"

// FS contém todos os arquivos .sql deste diretório.
//
//go:embed *.sql
var FS embed.FS
