package masquerade

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"

	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/service/filemanager"
)

func NewHandler(ctx context.Context, options *option.Hysteria2Masquerade) (http.Handler, error) {
	if options == nil || options.Type == "" {
		return nil, nil
	}
	switch options.Type {
	case C.Hysterai2MasqueradeTypeFile:
		directory := filemanager.BasePath(ctx, os.ExpandEnv(options.FileOptions.Directory))
		if _, err := filemanager.ReadDir(ctx, directory); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("read masquerade directory: %w", err)
		}
		return http.FileServer(http.Dir(directory)), nil
	case C.Hysterai2MasqueradeTypeProxy:
		target, err := url.Parse(options.ProxyOptions.URL)
		if err != nil {
			return nil, fmt.Errorf("parse masquerade URL: %w", err)
		}
		return &httputil.ReverseProxy{
			Rewrite: func(r *httputil.ProxyRequest) {
				r.SetURL(target)
				if !options.ProxyOptions.RewriteHost {
					r.Out.Host = r.In.Host
				}
			},
			ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
				w.WriteHeader(http.StatusBadGateway)
			},
		}, nil
	case C.Hysterai2MasqueradeTypeString:
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			for key, values := range options.StringOptions.Headers {
				for _, value := range values {
					w.Header().Add(key, value)
				}
			}
			if options.StringOptions.StatusCode != 0 {
				w.WriteHeader(options.StringOptions.StatusCode)
			}
			_, _ = w.Write([]byte(options.StringOptions.Content))
		}), nil
	default:
		return nil, fmt.Errorf("unknown masquerade type: %s", options.Type)
	}
}
