package hub_test

import (
	"net/http"
	"net/http/httptest"
)

// newH2CServer serves h over HTTP/1.1 and unencrypted HTTP/2, like the hub's public listener.
func newH2CServer(h http.Handler) *httptest.Server {
	srv := httptest.NewUnstartedServer(h)
	srv.Config.Protocols = new(http.Protocols)
	srv.Config.Protocols.SetHTTP1(true)
	srv.Config.Protocols.SetUnencryptedHTTP2(true)
	srv.Start()
	return srv
}

// h2cClient speaks HTTP/2 without TLS, which gRPC needs behind cloudflared's TLS termination.
func h2cClient() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Protocols = new(http.Protocols)
	t.Protocols.SetUnencryptedHTTP2(true)
	return &http.Client{Transport: t}
}
