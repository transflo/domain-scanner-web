// Package egress describes the network path a scan uses to reach registries: the host's own
// connection, or a local SOCKS5 port served by an Xray outbound.
package egress

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"

	xproxy "golang.org/x/net/proxy"
)

// DirectID is the reserved id of the host's own connection.
const DirectID = "direct"

const dialTimeout = 10 * time.Second

// Egress is one way out to the internet. The zero SocksAddr means "direct".
type Egress struct {
	ID        string
	Name      string
	SocksAddr string // host:port of a local SOCKS5 listener

	once   sync.Once
	dialer xproxy.ContextDialer

	mu      sync.Mutex
	clients map[time.Duration]*http.Client
}

// Direct returns the host's own connection.
func Direct() *Egress { return &Egress{ID: DirectID, Name: "直连"} }

// Socks returns an egress that tunnels through the SOCKS5 listener at addr.
func Socks(id, name, addr string) *Egress { return &Egress{ID: id, Name: name, SocksAddr: addr} }

func (e *Egress) IsDirect() bool { return e.SocksAddr == "" }

func (e *Egress) init() {
	e.once.Do(func() {
		base := &net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}
		if e.IsDirect() {
			e.dialer = base
			return
		}
		d, err := xproxy.SOCKS5("tcp", e.SocksAddr, nil, base)
		if err != nil {
			e.dialer = failDialer{err}
			return
		}
		if cd, ok := d.(xproxy.ContextDialer); ok {
			e.dialer = cd
		} else {
			e.dialer = failDialer{errors.New("socks5 dialer has no context support")}
		}
	})
}

type failDialer struct{ err error }

func (f failDialer) DialContext(context.Context, string, string) (net.Conn, error) { return nil, f.err }

// DialContext connects to addr ("host:port") through this egress. For SOCKS5 the host name is
// sent to the proxy unresolved, so name resolution also happens at the far end.
func (e *Egress) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	e.init()
	return e.dialer.DialContext(ctx, network, addr)
}

// Dial has the shape of golang.org/x/net/proxy.Dialer, which the WHOIS client expects.
func (e *Egress) Dial(network, addr string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
	defer cancel()
	return e.DialContext(ctx, network, addr)
}

// HTTPClient returns a pooled client that sends all traffic through this egress. Clients are
// cached per timeout so keep-alive connections are reused.
func (e *Egress) HTTPClient(timeout time.Duration) *http.Client {
	e.init()
	e.mu.Lock()
	defer e.mu.Unlock()
	if c, ok := e.clients[timeout]; ok {
		return c
	}
	if e.clients == nil {
		e.clients = map[time.Duration]*http.Client{}
	}
	c := &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			// Never the environment proxy: the egress alone decides the path.
			Proxy:                 nil,
			DialContext:           e.DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          20,
			IdleConnTimeout:       60 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: timeout,
		},
	}
	e.clients[timeout] = c
	return c
}
