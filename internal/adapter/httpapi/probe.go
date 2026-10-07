package httpapi

import (
	"context"
	"fmt"
	"net"
	"net/http"
)

// Probe chama /health/live na própria instância. É usado pelo subcomando
// "wallet-service healthcheck", porque a imagem distroless não tem curl.
//
// listenAddr é o mesmo valor de APP_HTTP_ADDR (ex.: ":8081"); um host vazio
// ou 0.0.0.0 vira 127.0.0.1.
func Probe(ctx context.Context, listenAddr string) error {
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return fmt.Errorf("endereço inválido %q: %w", listenAddr, err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	url := "http://" + net.JoinHostPort(host, port) + "/health/live"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s respondeu %d", url, resp.StatusCode)
	}
	return nil
}
