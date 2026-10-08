package hub

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestServeDrainsInFlightRequests(t *testing.T) {
	release := make(chan struct{})
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		_, _ = io.WriteString(w, "done")
	})}
	ln := listen(t)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- serve(ctx, srv, ln, 5*time.Second) }()

	type reply struct {
		body string
		err  error
	}
	got := make(chan reply, 1)
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/slow") //nolint:noctx // test
		if err != nil {
			got <- reply{err: err}
			return
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		got <- reply{body: string(b)}
	}()
	time.Sleep(100 * time.Millisecond) // let the request reach the handler
	cancel()                           // shutdown begins while the request is in flight
	time.Sleep(100 * time.Millisecond)
	select {
	case err := <-result:
		t.Fatalf("serve returned before the in-flight request finished: %v", err)
	default:
	}
	if _, err := dial(ln.Addr().String()); err == nil {
		t.Error("the listener should be closed to new connections during drain")
	}
	close(release)
	if r := <-got; r.err != nil || r.body != "done" {
		t.Errorf("in-flight request: %v %q", r.err, r.body)
	}
	if err := <-result; err != nil {
		t.Errorf("clean shutdown should return nil: %v", err)
	}
}

func TestServeTimesOutStuckRequests(t *testing.T) {
	block := make(chan struct{})
	srv := &http.Server{Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-block })}
	ln := listen(t)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- serve(ctx, srv, ln, 200*time.Millisecond) }()
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/stuck") //nolint:noctx // test
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	err := <-result
	close(block)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("want a deadline error from Shutdown, got %v", err)
	}
}

func listen(t *testing.T) net.Listener {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

func dial(addr string) (net.Conn, error) {
	d := net.Dialer{Timeout: time.Second}
	return d.DialContext(context.Background(), "tcp", addr)
}
