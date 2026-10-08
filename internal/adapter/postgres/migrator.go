package postgres

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5" // registra o esquema pgx5://
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

// Migrator aplica e reverte as migrations embutidas (ADR 0010). Deve ser
// usado com o role app_migrator, dono do schema.
type Migrator struct {
	m *migrate.Migrate
}

// NewMigrator conecta ao banco de databaseURL (postgres:// ou postgresql://)
// e usa os arquivos .sql de files como fonte.
func NewMigrator(files fs.FS, databaseURL string) (*Migrator, error) {
	src, err := iofs.New(files, ".")
	if err != nil {
		return nil, fmt.Errorf("ler migrations: %w", err)
	}
	url, err := pgx5URL(databaseURL)
	if err != nil {
		return nil, err
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, url)
	if err != nil {
		return nil, fmt.Errorf("conectar para migrations: %w", err)
	}
	return &Migrator{m: m}, nil
}

// pgx5URL troca o esquema da URL pelo do driver pgx v5 do golang-migrate.
func pgx5URL(databaseURL string) (string, error) {
	for _, prefix := range []string{"postgres://", "postgresql://"} {
		if rest, ok := strings.CutPrefix(databaseURL, prefix); ok {
			return "pgx5://" + rest, nil
		}
	}
	return "", errors.New("URL do banco deve começar com postgres:// ou postgresql://")
}

// Up aplica todas as migrations pendentes. Não há erro se já estiver em dia.
func (mg *Migrator) Up() error {
	if err := mg.m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return err
	}
	return nil
}

// Down reverte as últimas steps migrations.
func (mg *Migrator) Down(steps int) error {
	if steps < 1 {
		return fmt.Errorf("down exige pelo menos 1 passo (recebido %d)", steps)
	}
	v, _, err := mg.Version()
	if err != nil {
		return err
	}
	if v == 0 {
		return errors.New("nenhuma migration aplicada; nada a reverter")
	}

	err = mg.m.Steps(-steps)
	var short migrate.ErrShortLimit
	if errors.As(err, &short) {
		return fmt.Errorf("pedidos %d passos, mas só havia %d migrations aplicadas; todas foram revertidas", steps, uint(steps)-short.Short)
	}
	return err
}

// Version devolve a versão aplicada e se ela ficou "dirty" (uma migration
// falhou no meio). Sem nenhuma migration aplicada, devolve 0.
func (mg *Migrator) Version() (version uint, dirty bool, err error) {
	version, dirty, err = mg.m.Version()
	if errors.Is(err, migrate.ErrNilVersion) {
		return 0, false, nil
	}
	return version, dirty, err
}

// Force marca a versão como aplicada e limpa o estado dirty, sem executar
// SQL. Usado para recuperar uma migration que falhou no meio, depois de
// conferir e corrigir o banco à mão. Com -1, marca que nada foi aplicado.
func (mg *Migrator) Force(version int) error {
	return mg.m.Force(version)
}

// Close fecha a conexão com o banco e a fonte de arquivos.
func (mg *Migrator) Close() error {
	srcErr, dbErr := mg.m.Close()
	return errors.Join(srcErr, dbErr)
}
