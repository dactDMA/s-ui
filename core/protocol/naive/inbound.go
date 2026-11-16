package naive

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/alireza0/s-ui/core/protocol/masquerade"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/inbound"
	"github.com/sagernet/sing-box/common/listener"
	"github.com/sagernet/sing-box/common/tls"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	upstream "github.com/sagernet/sing-box/protocol/naive"
	_ "github.com/sagernet/sing-box/protocol/naive/quic"
	"github.com/sagernet/sing/common"
	"github.com/sagernet/sing/common/auth"
	N "github.com/sagernet/sing/common/network"
	aTLS "github.com/sagernet/sing/common/tls"
	sHTTP "github.com/sagernet/sing/protocol/http"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c" //nolint:staticcheck
)

type Options struct {
	option.NaiveInboundOptions
	Masquerade *option.Hysteria2Masquerade `json:"masquerade,omitempty"`
}

func RegisterInbound(registry *inbound.Registry) {
	inbound.Register[Options](registry, C.TypeNaive, NewInbound)
}

type Inbound struct {
	adapter.Inbound
	ctx           context.Context
	logger        log.ContextLogger
	options       option.NaiveInboundOptions
	listener      *listener.Listener
	tlsConfig     tls.ServerConfig
	proxy         http.Handler
	masquerade    http.Handler
	authenticator *auth.Authenticator
	http3Port     atomic.Uint32
}

func NewInbound(ctx context.Context, router adapter.Router, logger log.ContextLogger, tag string, options Options) (adapter.Inbound, error) {
	if options.Masquerade == nil || options.Masquerade.Type == "" {
		return upstream.NewInbound(ctx, router, logger, tag, options.NaiveInboundOptions)
	}
	if common.Contains(options.Network.Build(), N.NetworkUDP) && (options.TLS == nil || !options.TLS.Enabled) {
		return nil, errors.New("TLS is required for QUIC server")
	}
	if err := validateMasquerade(options.Masquerade); err != nil {
		return nil, err
	}
	website, err := masquerade.NewHandler(ctx, options.Masquerade)
	if err != nil {
		return nil, err
	}
	var tlsConfig tls.ServerConfig
	if options.TLS != nil {
		tlsConfig, err = tls.NewServer(ctx, logger, *options.TLS)
		if err != nil {
			return nil, err
		}
	}
	// Use sing-box for tunnel padding, routing, and accounting. This instance
	// only serves HTTP requests. The wrapper owns the listeners and TLS.
	proxyOptions := options.NaiveInboundOptions
	proxyOptions.Network = N.NetworkTCP
	proxyOptions.TLS = nil
	proxy, err := upstream.NewInbound(ctx, router, logger, tag, proxyOptions)
	if err != nil {
		return nil, err
	}
	return &Inbound{
		Inbound: proxy,
		ctx:     ctx,
		logger:  logger,
		options: options.NaiveInboundOptions,
		listener: listener.New(listener.Options{
			Context: ctx,
			Logger:  logger,
			Listen:  options.ListenOptions,
		}),
		tlsConfig:     tlsConfig,
		proxy:         proxy.(http.Handler),
		masquerade:    website,
		authenticator: auth.NewAuthenticator(options.Users),
	}, nil
}

func (n *Inbound) Start(stage adapter.StartStage, scope *adapter.Scope) error {
	if stage != adapter.StartStateStart {
		return nil
	}
	if n.tlsConfig != nil {
		if err := n.tlsConfig.Start(); err != nil {
			return fmt.Errorf("start Naive TLS: %w", err)
		}
		scope.Add(n.tlsConfig.Close)
	}
	scope.Add(n.listener.Close)

	network := n.options.Network.Build()
	var tcpListener net.Listener
	if common.Contains(network, N.NetworkTCP) {
		var err error
		tcpListener, err = n.listener.ListenTCP()
		if err != nil {
			return err
		}
		if n.tlsConfig != nil {
			protocols := n.tlsConfig.NextProtos()
			if len(protocols) == 0 {
				protocols = []string{http2.NextProtoTLS, "http/1.1"}
			} else if !common.Contains(protocols, http2.NextProtoTLS) {
				protocols = append([]string{http2.NextProtoTLS}, protocols...)
			}
			n.tlsConfig.SetNextProtos(protocols)
			tcpListener = aTLS.NewListener(tcpListener, n.tlsConfig)
		}
	}
	if common.Contains(network, N.NetworkUDP) {
		http3Listener, err := upstream.ConfigureHTTP3ListenerFunc(n.ctx, n.logger, n.listener, n, n.tlsConfig, n.options)
		if err != nil {
			if len(network) == 1 {
				return err
			}
			n.logger.Warn("naive http3 disabled: ", err)
		} else {
			scope.Add(http3Listener.Close)
			n.http3Port.Store(uint32(n.listener.UDPAddr().Port))
		}
	}
	if tcpListener != nil {
		n.serveTCP(tcpListener, scope)
	}
	return nil
}

func (n *Inbound) serveTCP(tcpListener net.Listener, scope *adapter.Scope) {
	server := &http.Server{
		// Use the same HTTP/1.1 and h2c handling as the upstream Naive inbound.
		Handler: h2c.NewHandler(n, &http2.Server{}), //nolint:staticcheck
		BaseContext: func(net.Listener) context.Context {
			return n.ctx
		},
	}
	scope.Add(server.Close)
	go func() {
		if err := server.Serve(tcpListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			n.logger.Error("naive http server closed: ", err)
		}
	}()
}

func (n *Inbound) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		name, password, ok := sHTTP.ParseBasicAuth(r.Header.Get("Proxy-Authorization"))
		if r.Header.Get("Padding") != "" && ok && n.authenticator.Verify(name, password) {
			n.proxy.ServeHTTP(w, r)
			return
		}
		// A website has no resource at a CONNECT authority. Never pass a
		// rejected tunnel request or its destination to the website upstream.
		http.NotFound(w, r)
		return
	}
	if port := n.http3Port.Load(); port != 0 && r.ProtoMajor < 3 {
		w.Header().Set("Alt-Svc", `h3=":`+strconv.FormatUint(uint64(port), 10)+`"; ma=86400`)
	}
	if r.Header.Get("Proxy-Authorization") != "" || r.Header.Get("Padding") != "" {
		r = r.Clone(r.Context())
		r.Header.Del("Proxy-Authorization")
		r.Header.Del("Padding")
	}
	n.masquerade.ServeHTTP(w, r)
}

func validateMasquerade(options *option.Hysteria2Masquerade) error {
	switch options.Type {
	case C.Hysterai2MasqueradeTypeFile:
		if strings.TrimSpace(os.ExpandEnv(options.FileOptions.Directory)) == "" {
			return errors.New("masquerade directory is required")
		}
	case C.Hysterai2MasqueradeTypeProxy:
		target, err := url.Parse(options.ProxyOptions.URL)
		if err != nil || target.Hostname() == "" || (target.Scheme != "http" && target.Scheme != "https") {
			return errors.New("masquerade requires an absolute HTTP or HTTPS URL")
		}
	case C.Hysterai2MasqueradeTypeString:
		status := options.StringOptions.StatusCode
		if status != 0 && (status < 200 || status > 599) {
			return errors.New("masquerade status code must be between 200 and 599")
		}
	}
	return nil
}
