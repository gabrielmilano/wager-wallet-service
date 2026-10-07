package logging

import (
	"io"
	"log/slog"
)

// New cria um logger JSON. Todo registro carrega o nome do serviço e o
// instanceId, para distinguir as instâncias quando várias rodam em paralelo.
func New(w io.Writer, level slog.Level, instanceID string) *slog.Logger {
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level})
	return slog.New(h).With(
		slog.String("service", "wager-wallet-service"),
		slog.String("instanceId", instanceID),
	)
}
