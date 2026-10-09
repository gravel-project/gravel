package hub

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
)

// listeners records the bound addresses so tests and operators can find ephemeral ports.
type listeners struct {
	started  chan struct{}
	once     sync.Once
	public   net.Addr
	internal net.Addr
}

func newListeners() *listeners { return &listeners{started: make(chan struct{})} }

// Started is closed once both listeners are bound.
func (h *Hub) Started() <-chan struct{} { return h.listeners.started }

// PublicAddr is the bound public address; valid after Started.
func (h *Hub) PublicAddr() net.Addr { return h.listeners.public }

// InternalAddr is the bound internal address; valid after Started.
func (h *Hub) InternalAddr() net.Addr { return h.listeners.internal }

// Run binds both listeners and serves until ctx is cancelled, then drains in-flight requests
// for at most server.shutdown_timeout. It closes the store before returning.
func (h *Hub) Run(ctx context.Context) error {
	defer h.Close()

	var lc net.ListenConfig
	publicLn, err := lc.Listen(ctx, "tcp", h.cfg.Server.Listen)
	if err != nil {
		return fmt.Errorf("hub: listen %s: %w", h.cfg.Server.Listen, err)
	}
	internalLn, err := lc.Listen(ctx, "tcp", h.cfg.Server.InternalListen)
	if err != nil {
		_ = publicLn.Close()
		return fmt.Errorf("hub: listen %s: %w", h.cfg.Server.InternalListen, err)
	}
	h.listeners.once.Do(func() {
		h.listeners.public, h.listeners.internal = publicLn.Addr(), internalLn.Addr()
		close(h.listeners.started)
	})
	h.logger.Info("listening", "public", publicLn.Addr().String(), "internal", internalLn.Addr().String(), "version", h.version)

	// gRPC needs HTTP/2, and TLS terminates at cloudflared, so the public listener speaks
	// unencrypted HTTP/2 (h2c) beside HTTP/1.1. Go 1.24+ does this natively.
	publicSrv := &http.Server{
		Handler:           h.public,
		Protocols:         publicProtocols(),
		ReadHeaderTimeout: 10 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	internalSrv := &http.Server{
		Handler:           h.internal,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errs := make(chan error, 2)
	go func() { errs <- serve(ctx, publicSrv, publicLn, h.cfg.Server.ShutdownTimeout) }()
	go func() { errs <- serve(ctx, internalSrv, internalLn, h.cfg.Server.ShutdownTimeout) }()
	jobsDone := make(chan struct{})
	go func() { h.jobs.Run(ctx); close(jobsDone) }()

	var first error
	for range 2 {
		if err := <-errs; err != nil && first == nil {
			first = err
			cancel() // one listener failed: bring the other down too
		}
	}
	cancel()
	<-jobsDone // no job touches the store after Run returns and closes it
	h.logger.Info("stopped")
	return first
}

func publicProtocols() *http.Protocols {
	p := new(http.Protocols)
	p.SetHTTP1(true)
	p.SetUnencryptedHTTP2(true)
	return p
}

// serve runs srv on ln until ctx is cancelled, then shuts it down gracefully within timeout.
// It returns Serve's error, or nil on a clean shutdown.
func serve(ctx context.Context, srv *http.Server, ln net.Listener, timeout time.Duration) error {
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	select {
	case err := <-served:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("hub: serve %s: %w", ln.Addr(), err)
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		_ = srv.Close()
		return fmt.Errorf("hub: shutdown %s: %w", ln.Addr(), err)
	}
	<-served
	return nil
}
