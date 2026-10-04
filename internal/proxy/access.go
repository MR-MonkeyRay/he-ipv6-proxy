package proxy

import (
	"context"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

// accessLog carries the per-request fields of one log line. The handler fills
// in the response status and size; the rotating dialer reports the source
// address it bound through the client trace.
type accessLog struct {
	start  time.Time
	status int
	bytes  int64
	src    atomic.Value // string; the bound source address once the dial is done
}

func newAccessLog() *accessLog { return &accessLog{start: time.Now()} }

func (a *accessLog) setSource(addr string) {
	if addr != "" {
		a.src.Store(addr)
	}
}

func (a *accessLog) source() string {
	s, _ := a.src.Load().(string)
	return s
}

// sourceAddr strips the ephemeral port from a bound address: for a rotating
// proxy the address is the interesting part, and the port is kernel-assigned.
func sourceAddr(a net.Addr) string {
	if a == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(a.String())
	if err != nil {
		return a.String()
	}
	return host
}

// accessLogKey is the context key under which the in-flight request's log
// fields travel to the outbound request.
type accessLogKey struct{}

func withAccessLog(ctx context.Context, a *accessLog) context.Context {
	return context.WithValue(ctx, accessLogKey{}, a)
}

func accessLogFrom(ctx context.Context) *accessLog {
	a, _ := ctx.Value(accessLogKey{}).(*accessLog)
	return a
}

// recorder records the status and body size of the response it wraps. It
// passes the real writer through Unwrap so http.ResponseController can still
// reach Flush and Hijack (protocol upgrades).
type recorder struct {
	http.ResponseWriter
	al *accessLog
}

func (rec *recorder) WriteHeader(code int) {
	if rec.al.status == 0 {
		rec.al.status = code
	}
	rec.ResponseWriter.WriteHeader(code)
}

func (rec *recorder) Write(b []byte) (int, error) {
	if rec.al.status == 0 {
		rec.al.status = http.StatusOK
	}
	n, err := rec.ResponseWriter.Write(b)
	rec.al.bytes += int64(n)
	return n, err
}

func (rec *recorder) Flush() {
	_ = http.NewResponseController(rec.ResponseWriter).Flush()
}

func (rec *recorder) Unwrap() http.ResponseWriter { return rec.ResponseWriter }

// logRequest writes the one line every plain-HTTP request produces, whatever
// the outcome: a request rejected with 407 or 400 never reaches the upstream
// and would otherwise leave no trace.
func (p *Proxy) logRequest(r *http.Request, al *accessLog) {
	p.log.Info("request",
		"client", r.RemoteAddr,
		"method", r.Method,
		"target", r.URL.Host,
		"path", r.URL.RequestURI(),
		"status", al.status,
		"bytes", al.bytes,
		"src", al.source(),
		"dur", time.Since(al.start).String(),
	)
}
