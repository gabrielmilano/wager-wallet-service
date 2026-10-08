package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/gabrielmilano/wager-wallet-service/internal/adapter/postgres"
	"github.com/gabrielmilano/wager-wallet-service/migrations"
)

const migrateUsage = `uso: wallet-service migrate <comando>

  up         aplica todas as migrations pendentes
  down [N]   reverte as últimas N migrations (padrão: 1)
  version    mostra a versão aplicada e se está dirty
  force V    marca a versão V como aplicada, sem executar SQL, e limpa o
             estado dirty (V = -1: nenhuma aplicada). Use só depois de
             conferir e corrigir o banco à mão.

Conecta com MIGRATIONS_DATABASE_URL (role app_migrator).`

// runMigrate executa o subcomando "migrate" e devolve o código de saída.
func runMigrate(args []string, stdout, stderr io.Writer) int {
	cmd, err := parseMigrateArgs(args)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n\n%s\n", err, migrateUsage)
		return 2
	}

	url := os.Getenv("MIGRATIONS_DATABASE_URL")
	if url == "" {
		fmt.Fprintln(stderr, "MIGRATIONS_DATABASE_URL é obrigatória")
		return 2
	}

	m, err := postgres.NewMigrator(migrations.FS, url)
	if err != nil {
		fmt.Fprintln(stderr, "migrate:", err)
		return 1
	}
	defer m.Close()

	switch cmd.name {
	case "up":
		err = m.Up()
	case "down":
		err = m.Down(cmd.n)
	case "force":
		err = m.Force(cmd.n)
	}
	if err != nil {
		fmt.Fprintf(stderr, "migrate %s: %v\n", cmd.name, err)
		return 1
	}

	v, dirty, err := m.Version()
	if err != nil {
		fmt.Fprintln(stderr, "migrate version:", err)
		return 1
	}
	fmt.Fprintf(stdout, "versão %d (dirty=%v)\n", v, dirty)
	if dirty {
		return 1
	}
	return 0
}

type migrateCmd struct {
	name string
	n    int
}

func parseMigrateArgs(args []string) (migrateCmd, error) {
	if len(args) == 0 {
		return migrateCmd{}, errors.New("comando ausente")
	}
	cmd := migrateCmd{name: args[0]}
	rest := args[1:]

	switch cmd.name {
	case "up", "version":
		if len(rest) != 0 {
			return migrateCmd{}, fmt.Errorf("%s não recebe argumentos", cmd.name)
		}
	case "down":
		cmd.n = 1
		if len(rest) > 1 {
			return migrateCmd{}, errors.New("down recebe no máximo um argumento")
		}
		if len(rest) == 1 {
			n, err := strconv.Atoi(rest[0])
			if err != nil || n < 1 {
				return migrateCmd{}, fmt.Errorf("down: N deve ser um inteiro >= 1 (recebido %q)", rest[0])
			}
			cmd.n = n
		}
	case "force":
		if len(rest) != 1 {
			return migrateCmd{}, errors.New("force exige a versão V")
		}
		v, err := strconv.Atoi(rest[0])
		if err != nil || v < -1 {
			return migrateCmd{}, fmt.Errorf("force: V deve ser um inteiro >= -1 (recebido %q)", rest[0])
		}
		cmd.n = v
	default:
		return migrateCmd{}, fmt.Errorf("comando desconhecido %q", cmd.name)
	}
	return cmd, nil
}
