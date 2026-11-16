package naive

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/sagernet/quic-go/http3"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/auth"
	"github.com/sagernet/sing/common/json/badoption"
	N "github.com/sagernet/sing/common/network"
	"golang.org/x/net/http2"
)

type testRouter struct {
	adapter.Router
	routed chan adapter.InboundContext
}

func (r testRouter) RouteConnectionEx(_ context.Context, conn net.Conn, metadata adapter.InboundContext, onClose N.CloseHandlerFunc) {
	r.routed <- metadata
	_ = conn.Close()
	onClose(nil)
}

func TestMasqueradeOnHTTP2AndHTTP3(t *testing.T) {
	probe, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	probe.Close()

	fixture := httptest.NewTLSServer(nil)
	pair := fixture.TLS.Certificates[0]
	fixture.Close()
	key, err := x509.MarshalPKCS8PrivateKey(pair.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	certificatePEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: pair.Certificate[0]})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(certificatePEM)

	var options Options
	options.ListenPort = uint16(port)
	options.Users = []auth.User{{Username: "alice", Password: "test-password"}}
	options.TLS = &option.InboundTLSOptions{
		Enabled:     true,
		Certificate: []string{string(certificatePEM)},
		Key:         []string{string(keyPEM)},
	}
	options.Masquerade = &option.Hysteria2Masquerade{
		Type: "string",
		StringOptions: option.Hysteria2MasqueradeString{
			StatusCode: http.StatusOK,
			Headers:    badoption.HTTPHeader{"Content-Type": {"text/html"}},
			Content:    "<h1>Test website</h1>",
		},
	}
	ctx := context.Background()
	logger := log.NewNOPFactory().NewLogger("test")
	router := testRouter{routed: make(chan adapter.InboundContext, 1)}
	inbound, err := NewInbound(ctx, router, logger, "naive-test", options)
	if err != nil {
		t.Fatal(err)
	}
	scope := adapter.NewScope(ctx, logger)
	t.Cleanup(func() { _ = scope.Close() })
	if err := inbound.Start(adapter.StartStateStart, scope); err != nil {
		t.Fatal(err)
	}
	if got := inbound.(*Inbound).http3Port.Load(); int(got) != port {
		t.Fatalf("HTTP/3 listener port = %d, want %d", got, port)
	}

	tlsConfig := &tls.Config{RootCAs: roots}
	h2 := &http2.Transport{TLSClientConfig: tlsConfig}
	h3 := &http3.Transport{TLSClientConfig: tlsConfig}
	t.Cleanup(h2.CloseIdleConnections)
	t.Cleanup(func() { _ = h3.Close() })
	endpoint := "https://127.0.0.1:" + strconv.Itoa(port)

	for _, transport := range []struct {
		name    string
		version int
		client  http.RoundTripper
	}{{"h2", 2, h2}, {"h3", 3, h3}} {
		t.Run(transport.name, func(t *testing.T) {
			client := &http.Client{Transport: transport.client, Timeout: 5 * time.Second}
			response, err := client.Get(endpoint)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil || response.StatusCode != http.StatusOK || response.ProtoMajor != transport.version || string(body) != "<h1>Test website</h1>" || response.Header.Get("Content-Type") != "text/html" {
				t.Fatalf("site: %s %s %q %v", response.Proto, response.Status, body, err)
			}
			if transport.version == 2 && response.Header.Get("Alt-Svc") == "" {
				t.Fatal("website did not advertise HTTP/3")
			}

			for _, credentials := range []struct {
				password string
				status   int
			}{{"wrong-password", http.StatusNotFound}, {"test-password", http.StatusOK}} {
				request, err := http.NewRequest(http.MethodConnect, endpoint, nil)
				if err != nil {
					t.Fatal(err)
				}
				request.SetBasicAuth("alice", credentials.password)
				request.Header.Set("Proxy-Authorization", request.Header.Get("Authorization"))
				request.Header.Del("Authorization")
				request.Header.Set("Padding", "test-padding")
				response, err := client.Do(request)
				if err != nil {
					t.Fatal(err)
				}
				response.Body.Close()
				if response.StatusCode != credentials.status {
					t.Fatalf("CONNECT: got %s, want %d", response.Status, credentials.status)
				}
				if credentials.status == http.StatusOK {
					select {
					case metadata := <-router.routed:
						if metadata.User != "alice" || metadata.Inbound != "naive-test" || response.Header.Get("Padding") == "" {
							t.Fatalf("CONNECT did not reach the Naive handler: %+v", metadata)
						}
					case <-time.After(5 * time.Second):
						t.Fatal("CONNECT was not routed")
					}
				}
			}
		})
	}
}
