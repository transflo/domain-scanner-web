package egress

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// socksServer is a minimal no-auth SOCKS5 server that records every CONNECT target.
type socksServer struct {
	ln      net.Listener
	mu      sync.Mutex
	targets []string
	conns   atomic.Int32
}

func newSocksServer(t *testing.T) *socksServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &socksServer{ln: ln}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.handle(c)
		}
	}()
	return s
}

func (s *socksServer) addr() string { return s.ln.Addr().String() }

func (s *socksServer) handle(c net.Conn) {
	defer c.Close()
	buf := make([]byte, 262)
	if _, err := io.ReadFull(c, buf[:2]); err != nil || buf[0] != 5 {
		return
	}
	if _, err := io.ReadFull(c, buf[:int(buf[1])]); err != nil {
		return
	}
	c.Write([]byte{5, 0})
	if _, err := io.ReadFull(c, buf[:4]); err != nil || buf[1] != 1 {
		return
	}
	var host string
	switch buf[3] {
	case 1:
		io.ReadFull(c, buf[:4])
		host = net.IP(buf[:4]).String()
	case 3:
		io.ReadFull(c, buf[:1])
		n := int(buf[0])
		io.ReadFull(c, buf[:n])
		host = string(buf[:n])
	default:
		return
	}
	io.ReadFull(c, buf[:2])
	port := binary.BigEndian.Uint16(buf[:2])
	target := fmt.Sprintf("%s:%d", host, port)
	s.mu.Lock()
	s.targets = append(s.targets, target)
	s.mu.Unlock()
	s.conns.Add(1)
	// "localhost" style names are resolved here, like a real remote proxy would
	up, err := net.DialTimeout("tcp", strings.Replace(target, "proxied.test", "127.0.0.1", 1), 3*time.Second)
	if err != nil {
		c.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	defer up.Close()
	c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
	go io.Copy(up, c)
	io.Copy(c, up)
}

func TestDirectEgressDialsNormally(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "hello") }))
	defer srv.Close()
	e := Direct()
	if !e.IsDirect() || e.ID != DirectID {
		t.Fatalf("direct egress = %+v", e)
	}
	resp, err := e.HTTPClient(2 * time.Second).Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "hello" {
		t.Fatalf("body = %q", b)
	}
}

func TestSocksEgressRoutesHTTPThroughTheProxy(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "via-proxy") }))
	defer site.Close()
	proxy := newSocksServer(t)
	e := Socks("proxy-1", "test proxy", proxy.addr())
	if e.IsDirect() {
		t.Fatal("socks egress reports direct")
	}
	_, port, _ := net.SplitHostPort(site.Listener.Addr().String())
	// the hostname only the "remote" proxy can resolve proves the client did not resolve it locally
	resp, err := e.HTTPClient(3 * time.Second).Get("http://proxied.test:" + port + "/")
	if err != nil {
		t.Fatalf("GET through proxy: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "via-proxy" {
		t.Fatalf("body = %q", b)
	}
	if proxy.conns.Load() == 0 || !strings.HasPrefix(proxy.targets[0], "proxied.test:") {
		t.Fatalf("proxy saw targets %v; the hostname must reach the proxy unresolved", proxy.targets)
	}
}

func TestSocksEgressDialContextAndWhoisStyleDial(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer site.Close()
	proxy := newSocksServer(t)
	e := Socks("p", "p", proxy.addr())
	c, err := e.DialContext(context.Background(), "tcp", site.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	c2, err := e.Dial("tcp", site.Listener.Addr().String()) // proxy.Dialer shape used by the whois client
	if err != nil {
		t.Fatal(err)
	}
	c2.Close()
	if proxy.conns.Load() != 2 {
		t.Fatalf("proxy connections = %d, want 2", proxy.conns.Load())
	}
}

func TestSocksEgressFailsFastWhenProxyIsDown(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close() // nothing listens any more
	e := Socks("dead", "dead", addr)
	start := time.Now()
	_, err := e.DialContext(context.Background(), "tcp", "example.com:80")
	if err == nil {
		t.Fatal("expected an error")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("took %v to fail", time.Since(start))
	}
}

func TestDialContextHonoursCancellation(t *testing.T) {
	// a proxy that accepts but never answers the handshake
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c
		}
	}()
	e := Socks("hang", "hang", ln.Addr().String())
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := e.DialContext(ctx, "tcp", "example.com:80"); err == nil {
		t.Fatal("expected an error")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("DialContext ignored the context deadline (%v)", time.Since(start))
	}
}

func TestHTTPClientIsCachedPerTimeout(t *testing.T) {
	e := Direct()
	if e.HTTPClient(time.Second) != e.HTTPClient(time.Second) {
		t.Fatal("same timeout must reuse the client (connection pooling)")
	}
	if e.HTTPClient(time.Second) == e.HTTPClient(2*time.Second) {
		t.Fatal("different timeouts must not share a client")
	}
}
