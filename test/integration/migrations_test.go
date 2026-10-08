//go:build integration

package integration

import (
	"context"
	"fmt"
	"io/fs"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gabrielmilano/wager-wallet-service/internal/adapter/postgres"
	"github.com/gabrielmilano/wager-wallet-service/migrations"
)

// TestMigrationsRoundTrip aplica todas as migrations, reverte todas e aplica
// de novo num banco temporário, para não afetar o banco compartilhado do
// Compose (ADR 0007). Prova que cada down desfaz exatamente o seu up.
func TestMigrationsRoundTrip(t *testing.T) {
	ctx := testContext(t)
	migratorURL := tempDatabase(t, ctx)
	total := countMigrations(t)

	m, err := postgres.NewMigrator(migrations.FS, migratorURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })

	requireVersion := func(want uint) {
		t.Helper()
		v, dirty, err := m.Version()
		if err != nil || v != want || dirty {
			t.Fatalf("versão = %d (dirty=%v, err=%v), want %d limpa", v, dirty, err, want)
		}
	}

	if err := m.Up(); err != nil {
		t.Fatalf("up: %v", err)
	}
	requireVersion(total)
	requireRuntimeGrants(t, ctx, migratorURL)

	if err := m.Down(int(total)); err != nil {
		t.Fatalf("down %d: %v", total, err)
	}
	requireVersion(0)
	requireEmptySchema(t, ctx, migratorURL)

	if err := m.Up(); err != nil {
		t.Fatalf("up de novo: %v", err)
	}
	requireVersion(total)
	requireRuntimeGrants(t, ctx, migratorURL)
}

// tempDatabase cria um banco vazio preparado como o initdb faz, mas sem
// recriar os roles (são globais do cluster e já existem). Devolve a URL de
// conexão como app_migrator e apaga o banco no fim do teste.
func tempDatabase(t *testing.T, ctx context.Context) string {
	t.Helper()
	admin := connect(t, ctx, env("POSTGRES_ADMIN_URL",
		"postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable"))

	name := fmt.Sprintf("wager_wallet_roundtrip_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+name+` OWNER app_migrator`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`)
	})
	if _, err := admin.Exec(ctx,
		`REVOKE ALL ON DATABASE `+name+` FROM PUBLIC;
		 GRANT CONNECT ON DATABASE `+name+` TO app_migrator, app_runtime`); err != nil {
		t.Fatal(err)
	}

	adminURL := withDatabase(t, env("POSTGRES_ADMIN_URL",
		"postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable"), name)
	db := connect(t, ctx, adminURL)
	if _, err := db.Exec(ctx,
		`ALTER SCHEMA public OWNER TO app_migrator;
		 REVOKE ALL ON SCHEMA public FROM PUBLIC;
		 GRANT USAGE ON SCHEMA public TO app_runtime`); err != nil {
		t.Fatal(err)
	}

	return withDatabase(t, env("MIGRATIONS_DATABASE_URL",
		"postgres://app_migrator:app_migrator@localhost:5432/wager_wallet?sslmode=disable"), name)
}

// withDatabase troca o nome do banco numa URL de conexão.
func withDatabase(t *testing.T, rawURL, database string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + database
	return u.String()
}

func countMigrations(t *testing.T) uint {
	t.Helper()
	ups, err := fs.Glob(migrations.FS, "*.up.sql")
	if err != nil || len(ups) == 0 {
		t.Fatalf("migrations embutidas: %d (err=%v)", len(ups), err)
	}
	return uint(len(ups))
}

// requireRuntimeGrants confere, por amostragem, as permissões que as
// migrations dão ao app_runtime.
func requireRuntimeGrants(t *testing.T, ctx context.Context, dbURL string) {
	t.Helper()
	conn := connect(t, ctx, dbURL)

	tests := []struct {
		table, privilege string
		want             bool
	}{
		{"wallet_ledger_entries", "INSERT", true},
		{"wallet_ledger_entries", "UPDATE", false},
		{"wallet_ledger_entries", "DELETE", false},
		{"wager_transactions", "DELETE", false},
		{"wager_transactions", "TRUNCATE", false},
		{"wallets", "DELETE", false},
		{"outbox_events", "INSERT", true},
		{"inbox_messages", "INSERT", true},
	}
	for _, tt := range tests {
		var got bool
		err := conn.QueryRow(ctx, `SELECT has_table_privilege('app_runtime', $1, $2)`,
			tt.table, tt.privilege).Scan(&got)
		if err != nil {
			t.Fatal(err)
		}
		if got != tt.want {
			t.Errorf("app_runtime %s em %s = %v, want %v", tt.privilege, tt.table, got, tt.want)
		}
	}
}

// requireEmptySchema confere que, depois de reverter tudo, só resta a tabela
// de controle do golang-migrate: nenhuma tabela ou função esquecida no down.
func requireEmptySchema(t *testing.T, ctx context.Context, dbURL string) {
	t.Helper()
	conn := connect(t, ctx, dbURL)

	rows, err := conn.Query(ctx,
		`SELECT 'tabela ' || tablename FROM pg_tables
		  WHERE schemaname = 'public' AND tablename <> 'schema_migrations'
		 UNION ALL
		 SELECT 'função ' || p.proname FROM pg_proc p
		   JOIN pg_namespace n ON n.oid = p.pronamespace
		  WHERE n.nspname = 'public'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var leftovers []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		leftovers = append(leftovers, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(leftovers) > 0 {
		t.Errorf("objetos restantes após down completo: %s", strings.Join(leftovers, ", "))
	}
}
