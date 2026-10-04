package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/MR-MonkeyRay/he-ipv6-proxy/internal/config"
)

func testConfig() *config.Config {
	c := config.Default()
	c.Network.IPv6Pool = "2001:db8::/64"
	c.Auth = config.AuthConfig{Enabled: true, User: "alice", Pass: "s3cret"}
	return c
}

func credentials(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

// captured is what the upstream server observed.
type captured struct {
	mu     sync.Mutex
	header http.Header
	url    string
	host   string
	close  bool
}

func (c *captured) record(r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.header = r.Header.Clone()
	c.url = r.URL.String()
	c.host = r.Host
	c.close = r.Close
}

func (c *captured) get() captured {
	c.mu.Lock()
	defer c.mu.Unlock()
	return captured{header: c.header, url: c.url, host: c.host, close: c.close}
}

func upstream(t *testing.T) (*httptest.Server, *captured) {
	t.Helper()
	c := &captured{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.record(r)
		w.Header().Set("X-Upstream", "yes")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hello from upstream"))
	}))
	t.Cleanup(srv.Close)
	return srv, c
}

func newProxy(t *testing.T, cfg *config.Config, dial DialFunc, logs *bytes.Buffer) *Proxy {
	t.Helper()
	var handler slog.Handler = slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})
	return New(cfg, dial, slog.New(handler))
}

var plainDial DialFunc = (&net.Dialer{}).DialContext

// TestRequestLogLine is the access-log contract: exactly one line per request,
// whatever the outcome, carrying the response status, the body size and the
// source address the dialer bound. Requests that never reach the upstream (407,
// 400) are the ones a transport-level log would miss.
func TestRequestLogLine(t *testing.T) {
	srv, _ := upstream(t)
	var logs bytes.Buffer
	dials := 0
	countDial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		dials++
		return plainDial(ctx, network, addr)
	}
	p := newProxy(t, testConfig(), countDial, &logs)

	req := httptest.NewRequest(http.MethodGet, srv.URL+"/p?a=1", nil)
	req.Header.Set("Proxy-Authorization", credentials("alice", "s3cret"))
	req.RemoteAddr = "192.0.2.7:41234"
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	line := lastLine(t, logs.String())
	for _, want := range []string{
		"msg=request", "client=192.0.2.7:41234", "method=GET",
		"target=" + srv.Listener.Addr().String(), `path="/p?a=1"`,
		"status=200", "bytes=19", "src=127.0.0.1 dur=",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("access line misses %q:\n%s", want, line)
		}
	}

	// A rejected request is logged too, with no source address: nothing was
	// dialed.
	logs.Reset()
	req = httptest.NewRequest(http.MethodGet, srv.URL+"/p", nil)
	req.RemoteAddr = "192.0.2.8:41235"
	p.ServeHTTP(httptest.NewRecorder(), req)
	line = lastLine(t, logs.String())
	for _, want := range []string{"client=192.0.2.8:41235", "status=407", `src=""`} {
		if !strings.Contains(line, want) {
			t.Errorf("407 access line misses %q:\n%s", want, line)
		}
	}

	// A malformed target never reaches the transport either.
	logs.Reset()
	req = httptest.NewRequest(http.MethodGet, "/relative", nil)
	req.Header.Set("Proxy-Authorization", credentials("alice", "s3cret"))
	p.ServeHTTP(httptest.NewRecorder(), req)
	if line = lastLine(t, logs.String()); !strings.Contains(line, "status=400") {
		t.Errorf("400 access line misses status=400:\n%s", line)
	}

	if dials != 1 {
		t.Errorf("upstream dials = %d, want 1 (only the authenticated request reaches the upstream)", dials)
	}
}

// lastLine returns the final non-empty log line.
func lastLine(t *testing.T, logs string) string {
	t.Helper()
	lines := strings.Split(strings.TrimRight(logs, "\n"), "\n")
	if len(lines) == 0 || lines[len(lines)-1] == "" {
		t.Fatalf("no log line, got:\n%s", logs)
	}
	return lines[len(lines)-1]
}

// TestPurePassthrough is the load-bearing check: the upstream must see the
// client's request and nothing added or leaked by the proxy.
func TestPurePassthrough(t *testing.T) {
	srv, cap := upstream(t)
	var logs bytes.Buffer
	p := newProxy(t, testConfig(), plainDial, &logs)

	req := httptest.NewRequest(http.MethodGet, srv.URL+"/p?a=1&b=2&c=%20x", nil)
	req.Header.Set("User-Agent", "curl/8.5.0")
	req.Header.Set("Proxy-Authorization", credentials("alice", "s3cret"))
	req.Header.Set("X-Custom", "keep-me")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "hello from upstream" {
		t.Fatalf("body = %q", got)
	}
	if got := rec.Header().Get("X-Upstream"); got != "yes" {
		t.Fatalf("upstream response header not relayed: %q", got)
	}

	got := cap.get()
	for _, h := range []string{"Via", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "Forwarded", "Proxy-Authorization", "Proxy-Connection"} {
		if v := got.header.Get(h); v != "" {
			t.Errorf("upstream saw leaked header %s: %q", h, v)
		}
	}
	if v := got.header.Get("User-Agent"); v != "curl/8.5.0" {
		t.Errorf("User-Agent = %q, want the client's curl/8.5.0", v)
	}
	if v := got.header.Get("X-Custom"); v != "keep-me" {
		t.Errorf("client header X-Custom = %q, want keep-me", v)
	}
	if want := "/p?a=1&b=2&c=%20x"; got.url != want {
		t.Errorf("upstream saw request URI %q, want %q", got.url, want)
	}
	if !got.close {
		t.Error("upstream connection was kept alive; per-request source rotation requires Connection: close")
	}

	// No client User-Agent => none upstream. Go's default must not appear.
	req2 := httptest.NewRequest(http.MethodGet, srv.URL+"/p", nil)
	req2.Header.Set("Proxy-Authorization", credentials("alice", "s3cret"))
	rec2 := httptest.NewRecorder()
	p.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("second request status = %d", rec2.Code)
	}
	if v := cap.get().header.Get("User-Agent"); v != "" {
		t.Errorf("User-Agent = %q, want no User-Agent when the client sent none", v)
	}

	if !strings.Contains(logs.String(), "status=200") || !strings.Contains(logs.String(), srv.Listener.Addr().String()) {
		t.Errorf("missing per-request log line, got:\n%s", logs.String())
	}
}

// TestOneDialPerRequest guards the rotation driver: if connections were pooled,
// a single upstream connection (and therefore one source address) would serve
// every request.
func TestOneDialPerRequest(t *testing.T) {
	srv, _ := upstream(t)
	var logs bytes.Buffer

	var mu sync.Mutex
	dials := 0
	countDial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		mu.Lock()
		dials++
		mu.Unlock()
		return plainDial(ctx, network, addr)
	}
	p := newProxy(t, testConfig(), countDial, &logs)

	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodGet, srv.URL+"/p", nil)
		req.Header.Set("Proxy-Authorization", credentials("alice", "s3cret"))
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d", i, rec.Code)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if dials != 3 {
		t.Fatalf("upstream dials = %d for 3 requests, want 3 (connections are being pooled)", dials)
	}
}

func TestRequestBodyAndResponseRelay(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("created"))
	}))
	t.Cleanup(srv.Close)

	var logs bytes.Buffer
	p := newProxy(t, testConfig(), plainDial, &logs)
	req := httptest.NewRequest(http.MethodPost, srv.URL+"/submit", strings.NewReader("payload=1"))
	req.Header.Set("Proxy-Authorization", credentials("alice", "s3cret"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", rec.Code)
	}
	if gotBody != "payload=1" {
		t.Errorf("upstream body = %q, want payload=1", gotBody)
	}
	if got := rec.Body.String(); got != "created" {
		t.Errorf("client body = %q, want created", got)
	}
}

func TestAuthRequired(t *testing.T) {
	srv, _ := upstream(t)
	var logs bytes.Buffer
	p := newProxy(t, testConfig(), plainDial, &logs)

	for name, hdr := range map[string]string{
		"absent":       "",
		"wrong pass":   credentials("alice", "nope"),
		"wrong user":   credentials("bob", "s3cret"),
		"not basic":    "Bearer token",
		"undecodable":  "Basic !!!!",
		"no separator": "Basic " + base64.StdEncoding.EncodeToString([]byte("alice")),
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, srv.URL+"/", nil)
			if hdr != "" {
				req.Header.Set("Proxy-Authorization", hdr)
			}
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, req)
			if rec.Code != http.StatusProxyAuthRequired {
				t.Fatalf("status = %d, want 407", rec.Code)
			}
			if got := rec.Header().Get("Proxy-Authenticate"); !strings.HasPrefix(got, "Basic ") {
				t.Errorf("Proxy-Authenticate = %q, want a Basic challenge", got)
			}
		})
	}
}

func TestAuthDisabledAllowsAnonymous(t *testing.T) {
	srv, cap := upstream(t)
	cfg := testConfig()
	cfg.Auth = config.AuthConfig{Enabled: false}
	var logs bytes.Buffer
	p := newProxy(t, cfg, plainDial, &logs)

	req := httptest.NewRequest(http.MethodGet, srv.URL+"/open", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with auth disabled", rec.Code)
	}
	if v := cap.get().header.Get("Proxy-Authorization"); v != "" {
		t.Errorf("leaked Proxy-Authorization: %q", v)
	}
}

// tcpUpstream accepts TCP connections, echoes everything back, and after the
// peer half-closes writes trailer before closing. It returns its address.
func tcpUpstream(t *testing.T, trailer string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 4096)
				for {
					n, err := c.Read(buf)
					if n > 0 {
						_, _ = c.Write(buf[:n])
					}
					if err != nil {
						_, _ = c.Write([]byte(trailer))
						return
					}
				}
			}(c)
		}
	}()
	return ln.Addr().String()
}

// serveOverTCP serves p on a real listener: CONNECT needs a hijackable
// connection, which httptest.NewRecorder cannot provide.
func serveOverTCP(t *testing.T, p *Proxy) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: p}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().String()
}

// TestConnectTunnel is the load-bearing CONNECT check: bytes must cross
// untouched in both directions, the client's half-close must reach the target,
// and the tunnel must open its own connection through the rotating dialer.
func TestConnectTunnel(t *testing.T) {
	const trailer = "upstream-after-fin"
	upAddr := tcpUpstream(t, trailer)

	var logs bytes.Buffer
	var mu sync.Mutex
	dials := 0
	var dialed string
	countDial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		mu.Lock()
		dials++
		dialed = addr
		mu.Unlock()
		return plainDial(ctx, network, addr)
	}
	p := newProxy(t, testConfig(), countDial, &logs)

	conn, err := net.Dial("tcp", serveOverTCP(t, p))
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()
	br := bufio.NewReader(conn)

	// The payload is pipelined behind the CONNECT request in a single write,
	// which is what real clients do: the TLS ClientHello follows immediately.
	payload := strings.Repeat("tls\x00\xff\x01", 64)
	head := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: %s\r\n\r\n",
		upAddr, upAddr, credentials("alice", "s3cret"))
	if _, err := conn.Write([]byte(head + payload)); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}

	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT status = %d, want 200", resp.StatusCode)
	}

	got := make([]byte, len(payload))
	if _, err := io.ReadFull(br, got); err != nil {
		t.Fatalf("read echoed payload: %v", err)
	}
	if string(got) != payload {
		t.Errorf("echoed payload = %q, want the %d bytes the client sent", got, len(payload))
	}

	if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}
	rest, err := io.ReadAll(br)
	if err != nil {
		t.Fatalf("read trailer: %v", err)
	}
	if string(rest) != trailer {
		t.Errorf("trailer = %q, want %q (the client's FIN did not reach the target)", rest, trailer)
	}

	mu.Lock()
	defer mu.Unlock()
	if dials != 1 {
		t.Errorf("upstream dials = %d, want 1 (the tunnel must open its own connection)", dials)
	}
	if dialed != upAddr {
		t.Errorf("dialed %q, want the CONNECT target %q", dialed, upAddr)
	}
}

func TestConnectAuthRequired(t *testing.T) {
	var logs bytes.Buffer
	dialed := false
	spyDial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		dialed = true
		return plainDial(ctx, network, addr)
	}
	p := newProxy(t, testConfig(), spyDial, &logs)

	req := httptest.NewRequest(http.MethodConnect, "example.com:443", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusProxyAuthRequired {
		t.Fatalf("CONNECT status = %d, want 407", rec.Code)
	}
	if dialed {
		t.Error("unauthenticated CONNECT reached the upstream dialer")
	}
	if line := lastLine(t, logs.String()); !strings.Contains(line, "msg=connect") || !strings.Contains(line, "status=407") {
		t.Errorf("rejected CONNECT left no access line:\n%s", line)
	}
}

func TestConnectTargetMustBeHostPort(t *testing.T) {
	var logs bytes.Buffer
	dialed := false
	spyDial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		dialed = true
		return plainDial(ctx, network, addr)
	}
	p := newProxy(t, testConfig(), spyDial, &logs)

	// Parsed off the wire, because these are what a real client can put in the
	// request line: no port, and a URL-form target that Go's parser reduces to
	// the degenerate authority "http:" (host "http", empty port). A non-numeric
	// port never reaches the handler: net/http rejects it while parsing.
	for _, raw := range []string{
		"CONNECT example.com HTTP/1.1\r\nHost: example.com\r\n\r\n",
		"CONNECT http://example.com:443 HTTP/1.1\r\nHost: example.com\r\n\r\n",
	} {
		req, err := http.ReadRequest(bufio.NewReader(strings.NewReader(raw)))
		if err != nil {
			t.Fatalf("parse %q: %v", raw, err)
		}
		req.Header.Set("Proxy-Authorization", credentials("alice", "s3cret"))
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("target %q: status = %d, want 400", req.URL.Host, rec.Code)
		}
	}
	if dialed {
		t.Error("dialer was called for a malformed CONNECT target")
	}
}

func TestConnectDialFailureIs502(t *testing.T) {
	var logs bytes.Buffer
	failDial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		return nil, errors.New("no route to host")
	}
	p := newProxy(t, testConfig(), failDial, &logs)

	req := httptest.NewRequest(http.MethodConnect, "[2001:db8::1]:443", nil)
	req.Header.Set("Proxy-Authorization", credentials("alice", "s3cret"))
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("CONNECT status = %d, want 502", rec.Code)
	}
	if !strings.Contains(logs.String(), "upstream failure") {
		t.Errorf("502 not logged, got:\n%s", logs.String())
	}
	if line := lastLine(t, logs.String()); !strings.Contains(line, "status=502") || !strings.Contains(line, "target=[2001:db8::1]:443") {
		t.Errorf("failed CONNECT access line = %q", line)
	}
}

func TestRejectsNonAbsoluteURI(t *testing.T) {
	var logs bytes.Buffer
	p := newProxy(t, testConfig(), plainDial, &logs)

	for _, target := range []string{"/relative", "https://example.com/"} {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		req.Header.Set("Proxy-Authorization", credentials("alice", "s3cret"))
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("target %q: status = %d, want 400", target, rec.Code)
		}
	}
}

func TestUpstreamFailureIs502(t *testing.T) {
	var logs bytes.Buffer
	failDial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		return nil, errors.New("no route to host")
	}
	p := newProxy(t, testConfig(), failDial, &logs)

	req := httptest.NewRequest(http.MethodGet, "http://target.invalid/x", nil)
	req.Header.Set("Proxy-Authorization", credentials("alice", "s3cret"))
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if !strings.Contains(logs.String(), "upstream failure") {
		t.Errorf("502 not logged, got:\n%s", logs.String())
	}
}
