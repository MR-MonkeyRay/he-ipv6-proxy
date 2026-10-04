// Package proxy implements the he-ipv6-proxy HTTP handler: a forward proxy
// (absolute-form plain HTTP plus CONNECT tunnels) that is a pure passthrough
// and adds no identifying headers.
package proxy

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/http/httputil"
	"strings"

	"github.com/MR-MonkeyRay/he-ipv6-proxy/internal/config"
)

// DialFunc dials the upstream. run passes netx.Rotator.Dial; tests pass a plain
// dialer so no IPv6 pool is needed.
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// Proxy is an http.Handler implementing an authenticated forward proxy.
type Proxy struct {
	rp   *httputil.ReverseProxy
	auth authChecker
	// dial opens CONNECT tunnels. It is the same rotating dialer the plain-HTTP
	// transport uses, so both paths draw one fresh pool address per connection.
	dial DialFunc
	log  *slog.Logger
}

// New assembles the handler. dialFn performs the upstream connection.
func New(cfg *config.Config, dialFn DialFunc, log *slog.Logger) *Proxy {
	tr := &http.Transport{
		DialContext: dialFn,
		// Required: a pooled connection would reuse the source address across
		// requests and defeat per-request rotation. The cost is one extra
		// "Connection: close" on the upstream request, which carries neither
		// proxy identity nor client IP.
		DisableKeepAlives: true,
		// Explicit: setting DialContext disables HTTP/2 unless this is set.
		ForceAttemptHTTP2:     false,
		ResponseHeaderTimeout: cfg.Network.ResponseHeaderTimeout.Std(),
	}

	rp := &httputil.ReverseProxy{
		Transport: tr,
		// Rewrite, not Director: the Director path re-adds X-Forwarded-For.
		// The Rewrite path strips Forwarded/X-Forwarded-* and adds nothing.
		Rewrite: func(pr *httputil.ProxyRequest) {
			// ReverseProxy runs cleanQueryParams on the outbound query;
			// restore the client's bytes exactly.
			pr.Out.URL.RawQuery = pr.In.URL.RawQuery
			// Report the source address the rotating dialer bound to the
			// request's log line. GotConn fires once per request: keep-alives
			// are disabled, so the connection is always freshly dialed.
			if al := accessLogFrom(pr.In.Context()); al != nil {
				trace := &httptrace.ClientTrace{GotConn: func(ci httptrace.GotConnInfo) {
					if ci.Conn != nil {
						al.setSource(sourceAddr(ci.Conn.LocalAddr()))
					}
				}}
				pr.Out = pr.Out.WithContext(httptrace.WithClientTrace(pr.Out.Context(), trace))
			}
		},
		ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelError),
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Error("upstream failure", "host", r.URL.Host, "err", err)
			http.Error(w, "Bad Gateway", http.StatusBadGateway)
		},
	}

	return &Proxy{
		rp: rp,
		auth: authChecker{
			enabled: cfg.Auth.Enabled,
			user:    cfg.Auth.User,
			pass:    cfg.Auth.Pass,
		},
		dial: dialFn,
		log:  log,
	}
}

// ServeHTTP handles forward-proxy requests: absolute-form plain HTTP and
// CONNECT tunnels. Every request produces exactly one log line, from here for
// plain HTTP and from serveConnect for tunnels.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.serveConnect(w, r)
		return
	}
	al := newAccessLog()
	defer p.logRequest(r, al)
	p.serveHTTP(&recorder{ResponseWriter: w, al: al}, r.WithContext(withAccessLog(r.Context(), al)))
}

// serveHTTP serves one absolute-form plain HTTP request.
func (p *Proxy) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Scheme != "http" || r.URL.Host == "" {
		http.Error(w, "bad request: expected an absolute http:// URI", http.StatusBadRequest)
		return
	}
	if !p.requireAuth(w, r) {
		return
	}
	p.rp.ServeHTTP(w, r)
}

// requireAuth answers 407 and reports false when the credentials are missing or
// wrong.
func (p *Proxy) requireAuth(w http.ResponseWriter, r *http.Request) bool {
	if p.auth.check(r) {
		return true
	}
	w.Header().Set("Proxy-Authenticate", `Basic realm="he-ipv6-proxy"`)
	http.Error(w, "proxy authentication required", http.StatusProxyAuthRequired)
	return false
}

// authChecker validates the single shared Basic credential.
type authChecker struct {
	enabled    bool
	user, pass string
}

func (a authChecker) check(r *http.Request) bool {
	if !a.enabled {
		return true
	}
	user, pass, ok := parseBasicAuth(r.Header.Get("Proxy-Authorization"))
	if !ok {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(user), []byte(a.user)) == 1 &&
		subtle.ConstantTimeCompare([]byte(pass), []byte(a.pass)) == 1
}

const basicScheme = "Basic "

// parseBasicAuth decodes a Basic credentials header value.
func parseBasicAuth(h string) (user, pass string, ok bool) {
	if len(h) < len(basicScheme) || !strings.EqualFold(h[:len(basicScheme)], basicScheme) {
		return "", "", false
	}
	dec, err := base64.StdEncoding.DecodeString(strings.TrimSpace(h[len(basicScheme):]))
	if err != nil {
		return "", "", false
	}
	user, pass, ok = strings.Cut(string(dec), ":")
	if !ok {
		return "", "", false
	}
	return user, pass, true
}
