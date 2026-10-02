package proxy

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// socksServer is a minimal no-auth SOCKS5 server recording every CONNECT target.
type socksServer struct {
	ln      net.Listener
	mu      sync.Mutex
	targets []string
	conns   atomic.Int32
}

func startSocks(t *testing.T, addr string) *socksServer {
	t.Helper()
	ln, err := net.Listen("tcp", addr)
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
	io.ReadFull(c, buf[:int(buf[1])])
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
	target := net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(buf[:2]))))
	s.mu.Lock()
	s.targets = append(s.targets, target)
	s.mu.Unlock()
	s.conns.Add(1)
	up, err := net.DialTimeout("tcp", target, 3*time.Second)
	if err != nil {
		c.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	defer up.Close()
	c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
	go io.Copy(up, c)
	io.Copy(c, up)
}

// testSite serves generate_204 and a Cloudflare-style trace.
func testSite(t *testing.T, status int, delay time.Duration) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(delay)
		if r.URL.Path == "/trace" {
			fmt.Fprint(w, "fl=1\nip=203.0.113.7\nts=1\nloc=HK\ncolo=HKG\n")
			return
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv
}
