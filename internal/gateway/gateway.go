// Package gateway serves the TRÅDFRI gateway's CoAP API over DTLS with pre-shared keys.
//
// Pairing works as on the real gateway: the app connects as Client_identity, keyed with
// the security code, and POSTs a name of its choosing to /15011/9063. The gateway answers
// with a fresh key for that name, and the app reconnects with the pair from then on.
package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strconv"
	"strings"
	"sync"
	"time"

	piondtls "github.com/pion/dtls/v3"
	"github.com/plgd-dev/go-coap/v3/dtls"
	"github.com/plgd-dev/go-coap/v3/message"
	"github.com/plgd-dev/go-coap/v3/message/codes"
	"github.com/plgd-dev/go-coap/v3/mux"
	coapnet "github.com/plgd-dev/go-coap/v3/net"
	"github.com/plgd-dev/go-coap/v3/options"
	udpClient "github.com/plgd-dev/go-coap/v3/udp/client"

	"github.com/stanmart/overgang/internal/bridge"
	"github.com/stanmart/overgang/internal/state"
	t "github.com/stanmart/overgang/internal/tradfri"
)

type Config struct {
	Listen       string
	SecurityCode string
	Firmware     string
	// Hex gateway ID without separators, e.g. "020000004f47".
	GatewayID string
	// Extra resources served as fixed JSON, keyed by path. Writes replace them.
	Resources map[string]json.RawMessage
}

type Server struct {
	cfg    Config
	bridge *bridge.Bridge
	store  *state.Store

	mu    sync.Mutex
	peers map[mux.Conn]*peer // connections holding observations
	addr  string
	// Gateway settings the app writes, such as the daylight saving rules. They are
	// kept in memory and echoed back; nothing acts on them.
	settings map[string]any
	// Current values of Config.Resources.
	resources map[string]json.RawMessage
	started   int64
}

// observedSettings are gateway info keys a real gateway on firmware 1.21.57 reports
// whose meaning is unknown or irrelevant here. They start out with that gateway's
// values (Alexa and Google pairing off) and the app may overwrite them.
var observedSettings = map[string]any{
	"9062": 0, "9082": false, "9092": 0, "9093": 0, "9105": 0, "9106": 0, "9107": 0,
	"9118": 0, "9146": 0, "9201": 1, "9204": 1, "9211": 0, "9234": 1, "9266": 0,
	// Daylight saving rules and 9236; the app writes these on every start.
	"9072": 3, "9073": 28, "9074": 1, "9075": 0, "9076": 10, "9077": 31, "9078": 1,
	"9079": 0, "9080": 60, "9236": 3600,
}

// uuid derives a stable gateway UUID (9200) from the gateway ID.
func (s *Server) uuid() string {
	h := sha1.Sum([]byte("overgang/" + s.cfg.GatewayID))
	return fmt.Sprintf("%x-%x-%x-%x-%x", h[0:4], h[4:6], h[6:8], h[8:10], h[10:16])
}

func New(cfg Config, br *bridge.Bridge, store *state.Store) *Server {
	s := &Server{
		cfg:       cfg,
		bridge:    br,
		store:     store,
		peers:     map[mux.Conn]*peer{},
		settings:  maps.Clone(observedSettings),
		started:   time.Now().Unix(),
		resources: maps.Clone(cfg.Resources),
	}
	if s.resources == nil {
		s.resources = map[string]json.RawMessage{}
	}
	br.OnChange(s.notify)
	return s
}

// DTLSConfig returns the server's DTLS settings: PSK only, offering the CCM-8 suite the
// real gateway uses first.
func DTLSConfig(psk func(identity string) ([]byte, error)) *piondtls.Config {
	return &piondtls.Config{
		PSK: func(identity []byte) ([]byte, error) {
			return psk(string(identity))
		},
		PSKIdentityHint: []byte{},
		CipherSuites: []piondtls.CipherSuiteID{
			piondtls.TLS_PSK_WITH_AES_128_CCM_8,
			piondtls.TLS_PSK_WITH_AES_128_CCM,
			piondtls.TLS_PSK_WITH_AES_128_GCM_SHA256,
			piondtls.TLS_PSK_WITH_AES_128_CBC_SHA256,
		},
		ExtendedMasterSecret: piondtls.RequestExtendedMasterSecret,
	}
}

func (s *Server) psk(identity string) ([]byte, error) {
	if identity == t.BootstrapIdentity {
		return []byte(s.cfg.SecurityCode), nil
	}
	if psk, ok := s.store.PSK(identity); ok {
		return []byte(psk), nil
	}
	slog.Warn("unknown identity tried to connect", "identity", identity)
	return nil, fmt.Errorf("unknown identity %q", identity)
}

// Addr returns the address the server listens on, once Run has started listening.
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addr
}

// Run serves until ctx is done. ready, if set, is closed once the socket is open.
func (s *Server) Run(ctx context.Context, ready chan<- struct{}) error {
	l, err := coapnet.NewDTLSListener("udp", s.cfg.Listen, DTLSConfig(s.psk))
	if err != nil {
		return err
	}
	defer l.Close()
	s.mu.Lock()
	s.addr = l.Addr().String()
	s.mu.Unlock()
	slog.Info("gateway listening", "addr", s.addr)

	srv := dtls.NewServer(
		options.WithMux(mux.HandlerFunc(s.handle)),
		// The app keeps its session open to receive observe notifications. The library
		// default drops a connection after 16 s without traffic.
		options.WithInactivityMonitor(time.Hour, func(cc *udpClient.Conn) { _ = cc.Close() }),
	)
	if ready != nil {
		close(ready)
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(l) }()
	select {
	case <-ctx.Done():
		srv.Stop()
		<-errc
		return nil
	case err := <-errc:
		return err
	}
}

// identity returns the PSK identity the peer authenticated with.
func identity(c mux.Conn) string {
	if dc, ok := c.NetConn().(*piondtls.Conn); ok {
		if st, ok := dc.ConnectionState(); ok {
			return string(st.IdentityHint)
		}
	}
	return ""
}

func (s *Server) handle(w mux.ResponseWriter, r *mux.Message) {
	// Acknowledgements and resets of our observe notifications reach the handler as
	// empty messages. Answering them makes the library send a fresh confirmable message,
	// which the app resets, which we would answer again, and so on.
	if r.Code() == codes.Empty {
		return
	}
	path, _ := r.Options().Path()
	path = strings.Trim(path, "/")
	body, _ := r.ReadBody()
	id := identity(w.Conn())
	log := slog.With("method", r.Code().String(), "path", path, "identity", id, "remote", w.Conn().RemoteAddr().String())
	if obs, err := r.Options().Observe(); err == nil {
		log = log.With("observe", obs)
	}
	if len(body) > 0 {
		log = log.With("body", string(body))
	}

	if id == t.BootstrapIdentity && !(r.Code() == codes.POST && path == t.PathGateway+"/"+t.PathAuth) {
		log.Warn("bootstrap identity may only pair")
		s.respond(w, codes.Unauthorized, nil)
		return
	}

	switch r.Code() {
	case codes.GET:
		code, payload := s.render(path)
		if code != codes.Content {
			if payload != nil {
				// An error the real gateway gives too.
				log.Debug("request", "code", code.String())
			} else {
				log.Warn("UNHANDLED request", "code", code.String())
			}
			s.respond(w, code, payload)
			return
		}
		log.Debug("request")
		obs, err := r.Options().Observe()
		switch {
		case err == nil && obs == 0:
			if seq, ok := s.observe(w.Conn(), r.Token(), path); ok {
				s.respond(w, code, payload, message.Option{ID: message.Observe, Value: observeValue(seq)})
			} else {
				// Answering without the Observe option tells the peer it isn't observing.
				log.Warn("too many observations on one connection")
				s.respond(w, code, payload)
			}
		case err == nil && obs == 1:
			s.unobserve(w.Conn(), r.Token(), path)
			s.respond(w, code, payload)
		default:
			s.respond(w, code, payload)
		}
	case codes.POST:
		if path == t.PathGateway+"/"+t.PathAuth {
			s.pair(w, body, log)
			return
		}
		if s.resource(path) != nil {
			// Not knowing what a POST to an extra resource should create, accept it so
			// the app goes on and its request shows up in the log.
			log.Warn("POST to extra resource accepted without effect")
			s.respond(w, codes.Created, nil)
			return
		}
		log.Warn("UNHANDLED request")
		s.respond(w, codes.MethodNotAllowed, nil)
	case codes.PUT:
		code := s.put(r.Context(), path, body)
		if code == codes.Changed {
			log.Info("request")
		} else {
			log.Warn("rejected write", "code", code.String())
		}
		s.respond(w, code, nil)
	default:
		log.Warn("UNHANDLED request")
		s.respond(w, codes.MethodNotAllowed, nil)
	}
}

func (s *Server) respond(w mux.ResponseWriter, code codes.Code, payload any, opts ...message.Option) {
	var err error
	if payload == nil {
		err = w.SetResponse(code, message.TextPlain, nil, opts...)
	} else {
		var b []byte
		if b, err = json.Marshal(payload); err == nil {
			err = w.SetResponse(code, message.AppJSON, bytes.NewReader(b), opts...)
		}
	}
	if err != nil {
		slog.Error("cannot respond", "err", err)
	}
}

func (s *Server) pair(w mux.ResponseWriter, body []byte, log *slog.Logger) {
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		s.respond(w, codes.BadRequest, nil)
		return
	}
	name, _ := req[t.AttrIdentity].(string)
	if name == "" || name == t.BootstrapIdentity {
		s.respond(w, codes.BadRequest, nil)
		return
	}
	psk := randomKey()
	if err := s.store.SetPSK(name, psk); err != nil {
		log.Error("cannot save identity", "err", err)
		s.respond(w, codes.InternalServerError, nil)
		return
	}
	log.Info("paired new identity", "new_identity", name)
	s.respond(w, codes.Created, map[string]any{t.AttrPSK: psk, t.AttrFirmware: s.cfg.Firmware})
}

// render returns the resource at path.
func (s *Server) resource(path string) json.RawMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resources[path]
}

func (s *Server) render(path string) (codes.Code, any) {
	if raw := s.resource(path); raw != nil {
		return codes.Content, raw
	}
	seg := strings.Split(path, "/")
	ints := make([]int, len(seg))
	for i, p := range seg {
		ints[i], _ = strconv.Atoi(p)
	}
	found := func(v any, ok bool) (codes.Code, any) {
		if !ok {
			return codes.NotFound, nil
		}
		return codes.Content, v
	}
	switch {
	case path == t.PathDevices:
		return codes.Content, s.bridge.DeviceIDs()
	case len(seg) == 2 && seg[0] == t.PathDevices:
		return found(s.bridge.Device(ints[1]))
	case path == t.PathGroups:
		return codes.Content, s.bridge.GroupIDs()
	case len(seg) == 2 && seg[0] == t.PathGroups:
		return found(s.bridge.Group(ints[1]))
	case path == t.PathMoods:
		return codes.Content, s.bridge.GroupIDs()
	case len(seg) == 2 && seg[0] == t.PathMoods:
		return found(s.bridge.MoodIDs(ints[1]))
	case len(seg) == 3 && seg[0] == t.PathMoods:
		return found(s.bridge.Mood(ints[1], ints[2]))
	case path == t.PathGateway+"/"+t.PathUnknown9124:
		// The app watches this on every start; a real gateway on firmware 1.21.57
		// answers with this error.
		return codes.InternalServerError, map[string]string{"r": "01"}
	case path == t.PathSmartTasks, path == t.PathNotifications:
		return codes.Content, []any{}
	case path == t.PathGateway+"/"+t.PathGatewayInfo:
		return codes.Content, s.gatewayInfo()
	}
	return codes.NotFound, nil
}

func (s *Server) gatewayInfo() map[string]any {
	now := time.Now()
	info := map[string]any{}
	s.mu.Lock()
	maps.Copy(info, s.settings)
	s.mu.Unlock()
	maps.Copy(info, map[string]any{
		t.AttrNTPServer:         "1.tradfri.pool.ntp.org",
		t.AttrFirmware:          s.cfg.Firmware,
		t.AttrCurrentTimeUnix:   now.Unix(),
		t.AttrCurrentTimeISO:    now.UTC().Format("2006-01-02T15:04:05.000000Z"),
		t.AttrCommissioningMode: 0,
		t.AttrOTAUpdateState:    0,
		t.AttrUpdateProgress:    0,
		t.AttrOTAType:           5,
		t.AttrFirstSetup:        s.started,
		t.AttrTimeSource:        1,
		t.AttrGatewayID:         s.cfg.GatewayID,
		t.AttrHomekitID:         "N/A",
		t.AttrGatewayUUID:       s.uuid(),
	})
	return info
}

func (s *Server) put(ctx context.Context, path string, body []byte) codes.Code {
	if s.resource(path) != nil {
		if !json.Valid(body) {
			return codes.BadRequest
		}
		s.mu.Lock()
		s.resources[path] = json.RawMessage(bytes.Clone(body))
		s.mu.Unlock()
		s.notify(path)
		return codes.Changed
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var req map[string]any
	if err := dec.Decode(&req); err != nil {
		return codes.BadRequest
	}
	if path == t.PathGateway+"/"+t.PathGatewayInfo {
		s.mu.Lock()
		maps.Copy(s.settings, req)
		s.mu.Unlock()
		return codes.Changed
	}
	seg := strings.Split(path, "/")
	if len(seg) != 2 {
		return codes.MethodNotAllowed
	}
	id, err := strconv.Atoi(seg[1])
	if err != nil {
		return codes.NotFound
	}
	switch seg[0] {
	case t.PathDevices:
		err = s.bridge.SetDevice(ctx, id, req)
	case t.PathGroups:
		err = s.bridge.SetGroup(ctx, id, req)
	default:
		return codes.MethodNotAllowed
	}
	switch {
	case err == nil:
		return codes.Changed
	case errors.Is(err, bridge.ErrNotFound):
		return codes.NotFound
	case errors.Is(err, bridge.ErrReadOnly):
		return codes.Forbidden
	case errors.Is(err, bridge.ErrBadRequest):
		return codes.BadRequest
	}
	slog.Error("backend call failed", "path", path, "err", err)
	return codes.ServiceUnavailable
}

func randomKey() string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}
