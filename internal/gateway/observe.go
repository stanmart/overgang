package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/plgd-dev/go-coap/v3/message"
	"github.com/plgd-dev/go-coap/v3/message/codes"
	"github.com/plgd-dev/go-coap/v3/mux"
)

const (
	// Observations one connection may hold. The app observes every device and group,
	// which is a few dozen in a home.
	maxObservationsPerConn = 256
	// How long a notification may wait for the peer's acknowledgement. Notifications
	// are confirmable, and a peer that stopped answering (a phone gone to sleep with the
	// app open) would otherwise hold the send until the connection times out.
	notificationTimeout = 10 * time.Second
)

// peer is one connection's observations, delivered by a goroutine of its own. A slow or
// silent peer only ever delays itself, and notify never blocks: it marks observations
// dirty, and the peer sends each one's latest state when it gets to it.
type peer struct {
	conn mux.Conn
	obs  map[string]*observation // keyed by path and token
	// Observations with a change not yet sent.
	dirty map[*observation]bool
	wake  chan struct{}
	done  chan struct{}
}

type observation struct {
	path  string
	token []byte
	seq   uint32
}

func observationKey(path string, token []byte) string {
	return path + "\x00" + string(token)
}

// observe registers an observation and returns the sequence number to answer with, or
// false when the connection already holds too many.
func (s *Server) observe(c mux.Conn, token []byte, path string) (uint32, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.peers[c]
	if p == nil {
		p = &peer{
			conn:  c,
			obs:   map[string]*observation{},
			dirty: map[*observation]bool{},
			wake:  make(chan struct{}, 1),
			done:  make(chan struct{}),
		}
		s.peers[c] = p
		c.AddOnClose(func() { s.dropPeer(c) })
		go s.deliver(p)
	}
	key := observationKey(path, token)
	if o := p.obs[key]; o != nil {
		return o.seq, true
	}
	if len(p.obs) >= maxObservationsPerConn {
		return 0, false
	}
	o := &observation{path: path, token: bytes.Clone(token), seq: 2}
	p.obs[key] = o
	return o.seq, true
}

func (s *Server) unobserve(c mux.Conn, token []byte, path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.peers[c]; p != nil {
		key := observationKey(path, token)
		delete(p.dirty, p.obs[key])
		delete(p.obs, key)
	}
}

func (s *Server) dropPeer(c mux.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.peers[c]; p != nil {
		delete(s.peers, c)
		close(p.done)
	}
}

// notify queues the current state of each path for its observers. It never blocks on
// the network, so it is safe to call from the backend's event loop.
func (s *Server) notify(paths ...string) {
	changed := map[string]bool{}
	for _, p := range paths {
		changed[p] = true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.peers {
		marked := false
		for _, o := range p.obs {
			if changed[o.path] {
				p.dirty[o] = true
				marked = true
			}
		}
		if marked {
			select {
			case p.wake <- struct{}{}:
			default:
			}
		}
	}
}

// deliver sends a peer's queued notifications until the connection goes away.
func (s *Server) deliver(p *peer) {
	for {
		select {
		case <-p.done:
			return
		case <-p.wake:
		}

		s.mu.Lock()
		var batch []observation
		for o := range p.dirty {
			o.seq++
			batch = append(batch, observation{path: o.path, token: o.token, seq: o.seq})
		}
		clear(p.dirty)
		s.mu.Unlock()

		for _, o := range batch {
			code, payload := s.render(o.path)
			if code != codes.Content {
				continue
			}
			body, err := json.Marshal(payload)
			if err != nil {
				continue
			}
			if err := s.sendNotification(p.conn, o, body); err != nil {
				if errors.Is(err, context.DeadlineExceeded) {
					// The peer stopped acknowledging. Drop the connection rather than keep
					// queueing for it; the app reconnects and observes again.
					slog.Warn("observer stopped answering, closing its connection", "remote", p.conn.RemoteAddr().String())
					_ = p.conn.Close()
					return
				}
				// Most likely a reset: the peer no longer wants this observation.
				slog.Debug("dropping observation", "path", o.path, "err", err)
				s.unobserve(p.conn, o.token, o.path)
			}
		}
	}
}

func (s *Server) sendNotification(c mux.Conn, o observation, body []byte) error {
	ctx, cancel := context.WithTimeout(c.Context(), notificationTimeout)
	defer cancel()
	m := c.AcquireMessage(ctx)
	defer c.ReleaseMessage(m)
	m.SetCode(codes.Content)
	m.SetToken(o.token)
	m.SetContentFormat(message.AppJSON)
	m.SetObserve(o.seq & 0xffffff)
	m.SetBody(bytes.NewReader(body))
	return c.WriteMessage(m)
}

func observeValue(seq uint32) []byte {
	buf := make([]byte, 4)
	n, _ := message.EncodeUint32(buf, seq)
	return buf[:n]
}
