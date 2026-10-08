package bootstrap

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"

	"github.com/gabrielmilano/wager-wallet-service/internal/adapter/postgres"
	"github.com/gabrielmilano/wager-wallet-service/internal/app/port"
	"github.com/gabrielmilano/wager-wallet-service/internal/app/store"
	"github.com/gabrielmilano/wager-wallet-service/internal/app/wagering"
	"github.com/gabrielmilano/wager-wallet-service/internal/app/wallets"
	"github.com/gabrielmilano/wager-wallet-service/internal/platform/config"
)

// postgresModule fornece o pool e o TxRunner. O pool é criado antes dos
// componentes que o usam, então o Fx o fecha depois deles (ordem inversa).
var postgresModule = fx.Module("postgres",
	fx.Provide(newPool, newTxRunner),
	// Força a criação do pool para validar o banco já na inicialização.
	fx.Invoke(func(*pgxpool.Pool) {}),
)

// newPool valida a dependência no início (Ping) e fecha as conexões no fim.
func newPool(lc fx.Lifecycle, cfg config.Config) (*pgxpool.Pool, error) {
	pool, err := postgres.NewPool(context.Background(), cfg.DatabaseURL, cfg.DBMaxConns)
	if err != nil {
		return nil, err
	}
	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error { return pool.Ping(ctx) },
		OnStop: func(context.Context) error {
			pool.Close()
			return nil
		},
	})
	return pool, nil
}

func newTxRunner(pool *pgxpool.Pool, cfg config.Config) store.TxRunner {
	return postgres.NewTxRunner(pool, cfg.DBLockTimeout)
}

// appModule fornece os casos de uso e as portas de tempo e identidade.
var appModule = fx.Module("app",
	fx.Provide(
		func() port.Clock { return port.SystemClock{} },
		func() port.IDGenerator { return port.UUIDv7{} },
		wagering.NewService,
		wallets.NewService,
	),
)
