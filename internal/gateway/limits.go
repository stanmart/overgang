package gateway

import (
	"context"
	"log/slog"
	"net"
	"sync"
)

// maxConns caps concurrent DTLS connections, handshaking or established. A home has a
// handful of phones; each holds one connection, plus a short-lived one while pairing.
const maxConns = 64

// limitedListener refuses connections beyond a cap, so a LAN peer opening handshakes
// from many source ports cannot make the server hold unbounded handshake state.
type limitedListener struct {
	inner interface {
		AcceptWithContext(ctx context.Context) (net.Conn, error)
		Close() error
	}
	slots chan struct{}
}

func newLimitedListener(inner interface {
	AcceptWithContext(ctx context.Context) (net.Conn, error)
	Close() error
}, n int) *limitedListener {
	return &limitedListener{inner: inner, slots: make(chan struct{}, n)}
}

func (l *limitedListener) AcceptWithContext(ctx context.Context) (net.Conn, error) {
	for {
		c, err := l.inner.AcceptWithContext(ctx)
		if err != nil {
			return nil, err
		}
		select {
		case l.slots <- struct{}{}:
			return &limitedConn{Conn: c, release: func() { <-l.slots }}, nil
		default:
			slog.Warn("too many connections, refusing one", "remote", c.RemoteAddr().String())
			_ = c.Close()
		}
	}
}

func (l *limitedListener) Close() error { return l.inner.Close() }

type limitedConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *limitedConn) Close() error {
	c.once.Do(c.release)
	return c.Conn.Close()
}

// Unwrap gives the DTLS connection back, for reading the peer's PSK identity.
func (c *limitedConn) Unwrap() net.Conn { return c.Conn }
