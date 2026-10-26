// Package proxy sits between the app and a real TRÅDFRI gateway and logs every CoAP
// message in plain text. It terminates DTLS on both sides and copies the decrypted
// datagrams unchanged, so observe and block-wise transfers pass through untouched.
//
// It can do this because it sees the pairing exchange in the clear: when the app asks
// the gateway for a key, the proxy records the identity and the key the gateway answers
// with, and uses that pair on both legs from then on.
package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"

	piondtls "github.com/pion/dtls/v3"
	"github.com/plgd-dev/go-coap/v3/message"
	"github.com/plgd-dev/go-coap/v3/message/codes"
	"github.com/plgd-dev/go-coap/v3/udp/coder"

	"github.com/stanmart/overgang/internal/gateway"
	"github.com/stanmart/overgang/internal/state"
	t "github.com/stanmart/overgang/internal/tradfri"
)

type Proxy struct {
	Listen       string
	Gateway      string // host:port of the real gateway
	SecurityCode string // the real gateway's
	Store        *state.Store
	// Capture, if set, receives one JSON line per message.
	Capture func(Record)
}

// Record is one logged CoAP message.
type Record struct {
	Direction string          `json:"dir"` // "app→gw" or "gw→app"
	Identity  string          `json:"identity"`
	Type      string          `json:"type"`
	Code      string          `json:"code"`
	MessageID int32           `json:"mid"`
	Token     string          `json:"token"`
	Path      string          `json:"path,omitempty"`
	Observe   *uint32         `json:"observe,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"`
	Raw       string          `json:"raw,omitempty"` // payload that isn't JSON
}

func (p *Proxy) psk(identity string) ([]byte, error) {
	if identity == t.BootstrapIdentity {
		return []byte(p.SecurityCode), nil
	}
	if psk, ok := p.Store.PSK(identity); ok {
		return []byte(psk), nil
	}
	slog.Warn("app used an identity the proxy never saw paired; re-pair the app through the proxy", "identity", identity)
	return nil, fmt.Errorf("unknown identity %q", identity)
}

func (p *Proxy) Run(ctx context.Context) error {
	laddr, err := net.ResolveUDPAddr("udp", p.Listen)
	if err != nil {
		return err
	}
	l, err := piondtls.Listen("udp", laddr, gateway.DTLSConfig(func(id string) ([]byte, error) { return p.psk(id) }))
	if err != nil {
		return err
	}
	context.AfterFunc(ctx, func() { l.Close() })
	slog.Info("proxy listening", "addr", l.Addr().String(), "gateway", p.Gateway)
	for {
		conn, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go p.serve(ctx, conn)
	}
}

func (p *Proxy) serve(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	app := conn.(*piondtls.Conn)
	if err := app.HandshakeContext(ctx); err != nil {
		slog.Warn("app handshake failed", "remote", conn.RemoteAddr(), "err", err)
		return
	}
	st, _ := app.ConnectionState()
	identity := string(st.IdentityHint)
	key, err := p.psk(identity)
	if err != nil {
		return
	}

	raddr, err := net.ResolveUDPAddr("udp", p.Gateway)
	if err != nil {
		slog.Error("cannot resolve gateway", "err", err)
		return
	}
	gw, err := piondtls.Dial("udp", raddr, &piondtls.Config{
		PSK:                  func([]byte) ([]byte, error) { return key, nil },
		PSKIdentityHint:      []byte(identity),
		CipherSuites:         []piondtls.CipherSuiteID{piondtls.TLS_PSK_WITH_AES_128_CCM_8},
		ExtendedMasterSecret: piondtls.RequestExtendedMasterSecret,
	})
	if err != nil {
		slog.Error("cannot reach gateway", "err", err)
		return
	}
	defer gw.Close()
	if err := gw.HandshakeContext(ctx); err != nil {
		slog.Error("gateway handshake failed", "identity", identity, "err", err)
		return
	}
	slog.Info("app session", "identity", identity, "remote", conn.RemoteAddr())

	s := &session{proxy: p, identity: identity, pairing: map[string]string{}}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); s.pump(app, gw, "app→gw"); gw.Close() }()
	go func() { defer wg.Done(); s.pump(gw, app, "gw→app"); app.Close() }()
	wg.Wait()
	slog.Info("app session ended", "identity", identity)
}

type session struct {
	proxy    *Proxy
	identity string

	mu      sync.Mutex
	pairing map[string]string // token → identity being paired
}

func (s *session) pump(src, dst net.Conn, dir string) {
	buf := make([]byte, 64*1024)
	for {
		n, err := src.Read(buf)
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				slog.Debug("pump stopped", "dir", dir, "err", err)
			}
			return
		}
		s.inspect(dir, buf[:n])
		if _, err := dst.Write(buf[:n]); err != nil {
			return
		}
	}
}

func (s *session) inspect(dir string, data []byte) {
	m := message.Message{Options: make(message.Options, 0, 32)}
	if _, err := coder.DefaultCoder.Decode(data, &m); err != nil {
		slog.Warn("undecodable datagram", "dir", dir, "len", len(data), "err", err)
		return
	}
	rec := Record{
		Direction: dir,
		Identity:  s.identity,
		Type:      m.Type.String(),
		Code:      m.Code.String(),
		MessageID: m.MessageID,
		Token:     fmt.Sprintf("%x", []byte(m.Token)),
	}
	if path, err := m.Options.Path(); err == nil {
		rec.Path = strings.TrimPrefix(path, "/")
	}
	if obs, err := m.Options.Observe(); err == nil {
		rec.Observe = &obs
	}
	if len(m.Payload) > 0 {
		if json.Valid(m.Payload) {
			rec.Payload = json.RawMessage(m.Payload)
		} else {
			rec.Raw = fmt.Sprintf("%q", m.Payload)
		}
	}
	s.sniffPairing(rec, m)

	attrs := []any{"dir", dir, "code", rec.Code, "mid", rec.MessageID, "token", rec.Token}
	if rec.Path != "" {
		attrs = append(attrs, "path", rec.Path)
	}
	if rec.Observe != nil {
		attrs = append(attrs, "observe", *rec.Observe)
	}
	if len(m.Payload) > 0 {
		attrs = append(attrs, "payload", string(m.Payload))
	}
	slog.Info("coap", attrs...)
	if s.proxy.Capture != nil {
		s.proxy.Capture(rec)
	}
}

// sniffPairing learns identity/key pairs from the app's pairing exchange.
func (s *session) sniffPairing(rec Record, m message.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec.Direction == "app→gw" && m.Code == codes.POST && rec.Path == t.PathGateway+"/"+t.PathAuth {
		var body map[string]any
		if json.Unmarshal(m.Payload, &body) == nil {
			if id, _ := body[t.AttrIdentity].(string); id != "" {
				s.pairing[rec.Token] = id
			}
		}
		return
	}
	id, ok := s.pairing[rec.Token]
	if rec.Direction != "gw→app" || !ok || m.Code != codes.Created {
		return
	}
	delete(s.pairing, rec.Token)
	var body map[string]any
	if json.Unmarshal(m.Payload, &body) != nil {
		return
	}
	if psk, _ := body[t.AttrPSK].(string); psk != "" {
		if err := s.proxy.Store.SetPSK(id, psk); err != nil {
			slog.Error("cannot save sniffed identity", "err", err)
			return
		}
		slog.Info("learned identity from pairing", "identity", id)
	}
}
