package httpapi

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func TestLive(t *testing.T) {
	rec := httptest.NewRecorder()
	NewRouter(Deps{}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health/live", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != `{"status":"ok"}` {
		t.Errorf("body = %q", got)
	}
}

func TestLiveRejectsOtherMethods(t *testing.T) {
	rec := httptest.NewRecorder()
	NewRouter(Deps{}).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/health/live", nil))

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}

func TestServerStartProbeStop(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	srv := NewServer("127.0.0.1:0", NewRouter(Deps{}), discardLogger(), func(err error) {
		t.Errorf("onError chamado: %v", err)
	})
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if err := Probe(ctx, srv.Addr().String()); err != nil {
		t.Errorf("Probe com servidor no ar: %v", err)
	}

	if err := srv.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := Probe(ctx, srv.Addr().String()); err == nil {
		t.Error("Probe depois do Stop: esperado erro, veio nil")
	}
}

func TestServerStartFailsWhenPortIsTaken(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	srv := NewServer(ln.Addr().String(), NewRouter(Deps{}), discardLogger(), func(error) {})
	if err := srv.Start(context.Background()); err == nil {
		t.Error("Start em porta ocupada: esperado erro, veio nil")
	}
}

func TestProbeRejectsNon200(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, "fora")
	}))
	defer ts.Close()

	addr := ts.Listener.Addr().String()
	if err := Probe(context.Background(), addr); err == nil {
		t.Error("Probe com 503: esperado erro, veio nil")
	}
}

func TestProbeRewritesWildcardHost(t *testing.T) {
	srv := NewServer("127.0.0.1:0", NewRouter(Deps{}), discardLogger(), func(error) {})
	if err := srv.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop(context.Background())

	_, port, _ := net.SplitHostPort(srv.Addr().String())
	if err := Probe(context.Background(), ":"+port); err != nil {
		t.Errorf("Probe(%q): %v", ":"+port, err)
	}
}
