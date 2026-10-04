package proxy

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"
)

// drainGrace bounds how long the handler stays alive for the second copy
// direction after the first one finished. Without it a peer that never sends
// FIN would pin the handler (and the tunnel's source address) forever.
const drainGrace = 30 * time.Second

// serveConnect splices an opaque TCP stream between the client and target. TLS
// stays end-to-end: the proxy relays bytes and interprets none of them.
//
// The tunnel's single upstream connection is opened through p.dial, so it
// carries one fresh pool address. Rotation granularity is therefore the
// connection, not the request: a client that opens one tunnel per request
// rotates exactly like the plain-HTTP path, while a client that keeps a tunnel
// open (connection pooling, WebSocket) keeps one source address for its
// lifetime.
func (p *Proxy) serveConnect(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	// target is the raw authority until connectTarget validates it, so a
	// rejected request still names what the client asked for.
	target := r.URL.Host
	var (
		status     int // 0 = nothing was written back to the client
		sent, recv int64
		src        string
	)
	defer func() {
		p.log.Info("connect", "client", r.RemoteAddr, "target", target, "status", status,
			"src", src, "sent", sent, "received", recv, "dur", time.Since(start).String())
	}()

	if !p.requireAuth(w, r) {
		status = http.StatusProxyAuthRequired
		return
	}
	t, err := connectTarget(r)
	if err != nil {
		status = http.StatusBadRequest
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	target = t

	// Dial before hijacking: while the ResponseWriter is still usable, a failed
	// dial reaches the client as a clean 502 instead of a truncated tunnel.
	up, err := p.dial(r.Context(), "tcp", target)
	if err != nil {
		status = http.StatusBadGateway
		p.log.Error("upstream failure", "target", target, "err", err)
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
		return
	}
	defer up.Close()
	// The tunnel's source address, which the rotating dialer bound.
	src = sourceAddr(up.LocalAddr())

	hj, ok := w.(http.Hijacker)
	if !ok {
		status = http.StatusInternalServerError
		p.log.Error("connection is not hijackable", "client", r.RemoteAddr)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	client, brw, err := hj.Hijack()
	if err != nil {
		status = http.StatusInternalServerError
		p.log.Error("hijack failed", "client", r.RemoteAddr, "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	defer client.Close()

	// The tunnel has no framing, so the reply is written on the raw connection.
	if _, err := brw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	if err := brw.Flush(); err != nil {
		return
	}
	status = http.StatusOK

	// brw.Reader is the client side: bytes the client pipelined behind CONNECT
	// (a TLS ClientHello, typically) are already buffered in it.
	sentC := make(chan int64, 1)
	receivedC := make(chan int64, 1)
	go func() {
		n, _ := io.Copy(up, brw.Reader)
		closeWrite(up)
		sentC <- n
	}()
	go func() {
		n, _ := io.Copy(client, up)
		closeWrite(client)
		receivedC <- n
	}()

	// Whichever direction finishes first has already relayed the FIN to its
	// peer via closeWrite; give the opposite direction a bounded window to
	// drain what is still in flight before both sockets are closed.
	select {
	case sent = <-sentC:
		select {
		case recv = <-receivedC:
		case <-time.After(drainGrace):
		}
	case recv = <-receivedC:
		select {
		case sent = <-sentC:
		case <-time.After(drainGrace):
		}
	}
}

// connectTarget returns the authority a CONNECT request names. net/http parses
// authority-form into URL.Host, but it also accepts degenerate values such as
// "http:" (host "http", empty port), so the port is validated here.
func connectTarget(r *http.Request) (string, error) {
	host, port, err := net.SplitHostPort(r.URL.Host)
	if err != nil || host == "" {
		return "", errors.New("CONNECT target must be host:port")
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return "", fmt.Errorf("CONNECT target port %q is not numeric", port)
	}
	return net.JoinHostPort(host, port), nil
}

// closeWrite half-closes c so its peer sees EOF while the opposite direction
// keeps flowing; a full Close would cut off data still in flight.
func closeWrite(c net.Conn) {
	type closeWriter interface{ CloseWrite() error }
	if cw, ok := c.(closeWriter); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = c.Close()
}
