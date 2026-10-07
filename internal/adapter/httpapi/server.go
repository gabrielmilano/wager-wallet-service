package httpapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// Server envolve o http.Server com início e parada explícitos, para que o
// ciclo de vida seja controlado de fora (fx.Lifecycle em internal/bootstrap).
type Server struct {
	srv     *http.Server
	log     *slog.Logger
	onError func(error)

	addr net.Addr      // endereço real após Start (útil com porta :0)
	done chan struct{} // fechado quando Serve retorna
}

// NewServer cria o servidor. onError é chamado se o servidor parar por um
// erro inesperado depois de iniciado, para que a aplicação inteira encerre em
// vez de seguir sem HTTP.
func NewServer(addr string, h http.Handler, log *slog.Logger, onError func(error)) *Server {
	return &Server{
		srv: &http.Server{
			Addr:              addr,
			Handler:           h,
			ReadHeaderTimeout: 5 * time.Second,
		},
		log:     log,
		onError: onError,
		done:    make(chan struct{}),
	}
}

// Start abre a porta de forma síncrona, para falhar já na inicialização se
// ela estiver ocupada, e atende as requisições em uma goroutine.
func (s *Server) Start(ctx context.Context) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", s.srv.Addr)
	if err != nil {
		return fmt.Errorf("abrir %s: %w", s.srv.Addr, err)
	}
	s.addr = ln.Addr()
	s.log.Info("servidor HTTP iniciado", slog.String("addr", s.addr.String()))

	go func() {
		defer close(s.done)
		if err := s.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Error("servidor HTTP parou com erro", slog.Any("error", err))
			s.onError(err)
		}
	}()
	return nil
}

// Stop para de aceitar conexões e espera as requisições em andamento
// terminarem, até o prazo de ctx.
func (s *Server) Stop(ctx context.Context) error {
	err := s.srv.Shutdown(ctx)
	select {
	case <-s.done:
	case <-ctx.Done():
	}
	s.log.Info("servidor HTTP encerrado")
	return err
}

// Addr devolve o endereço em que o servidor está escutando (nil antes de
// Start).
func (s *Server) Addr() net.Addr {
	return s.addr
}
