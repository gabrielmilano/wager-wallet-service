// Command wallet-service executa o serviço de carteiras: servidor HTTP,
// consumidor SQS e workers, conforme as variáveis APP_ENABLE_* (ADR 0002).
//
// Uso:
//
//	wallet-service              inicia a aplicação
//	wallet-service healthcheck  verifica a liveness da instância local
//	wallet-service migrate ...  aplica ou reverte as migrations (ADR 0010)
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"go.uber.org/fx"

	"github.com/gabrielmilano/wager-wallet-service/internal/adapter/httpapi"
	"github.com/gabrielmilano/wager-wallet-service/internal/bootstrap"
	"github.com/gabrielmilano/wager-wallet-service/internal/platform/config"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "healthcheck":
			os.Exit(healthcheck())
		case "migrate":
			os.Exit(runMigrate(os.Args[2:], os.Stdout, os.Stderr))
		}
	}

	cfg, err := config.Load(os.LookupEnv)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	// Run inicia a aplicação, espera SIGINT/SIGTERM e encerra respeitando
	// o StopTimeout. Se a inicialização falhar, sai com código 1.
	fx.New(bootstrap.Options(cfg)).Run()
}

// healthcheck existe porque a imagem distroless não tem curl nem shell: o
// healthcheck do Docker executa o próprio binário.
func healthcheck() int {
	addr := os.Getenv("APP_HTTP_ADDR")
	if addr == "" {
		addr = config.DefaultHTTPAddr
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := httpapi.Probe(ctx, addr); err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	return 0
}
