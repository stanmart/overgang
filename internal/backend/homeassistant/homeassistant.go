// Package homeassistant is the real backend: Home Assistant's websocket API. It reads
// every state once, follows state_changed events, and calls services. Lights in Zigbee2MQTT
// go through it too, so a Z2M group exposed as a Home Assistant light still switches a
// whole room with one Zigbee command, and transitions are passed on unchanged.
package homeassistant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/stanmart/overgang/internal/backend"
)

// Backend talks to one Home Assistant instance.
type Backend struct {
	url   string
	token string

	mu      sync.Mutex // guards conn, nextID and pending; also serialises writes
	conn    *websocket.Conn
	nextID  int
	pending map[int]chan result
}

type result struct {
	Success bool `json:"success"`
	Error   *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Result json.RawMessage `json:"result"`
}

type state struct {
	EntityID   string         `json:"entity_id"`
	State      string         `json:"state"`
	Attributes map[string]any `json:"attributes"`
}

// New makes a backend for the websocket URL, for example
// ws://127.0.0.1:8123/api/websocket, authenticating with a long-lived access token.
func New(url, token string) *Backend {
	return &Backend{url: url, token: token}
}

func (b *Backend) Run(ctx context.Context, watch []string, update func(backend.Entity)) error {
	watched := map[string]bool{}
	for _, id := range watch {
		watched[id] = true
	}
	backoff := time.Second
	for {
		start := time.Now()
		err := b.session(ctx, watched, update)
		if ctx.Err() != nil {
			return nil
		}
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		slog.Warn("home assistant connection lost, reconnecting", "err", err, "in", backoff)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

// session runs one websocket connection until it fails.
func (b *Backend) session(ctx context.Context, watched map[string]bool, update func(backend.Entity)) error {
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, b.url, nil)
	if err != nil {
		return err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	if err := authenticate(conn, b.token); err != nil {
		return err
	}
	slog.Info("connected to home assistant", "url", b.url)

	b.mu.Lock()
	b.conn, b.nextID, b.pending = conn, 1, map[int]chan result{}
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		b.conn = nil
		for _, ch := range b.pending {
			close(ch)
		}
		b.pending = nil
		b.mu.Unlock()
	}()

	statesID, err := b.send(map[string]any{"type": "get_states"})
	if err != nil {
		return err
	}
	if _, err := b.send(map[string]any{"type": "subscribe_events", "event_type": "state_changed"}); err != nil {
		return err
	}

	for {
		var msg struct {
			ID    int    `json:"id"`
			Type  string `json:"type"`
			Event struct {
				Data struct {
					NewState *state `json:"new_state"`
				} `json:"data"`
			} `json:"event"`
			result
		}
		if err := conn.ReadJSON(&msg); err != nil {
			return err
		}
		switch msg.Type {
		case "event":
			if s := msg.Event.Data.NewState; s != nil && watched[s.EntityID] {
				update(backend.Entity{ID: s.EntityID, State: s.State, Attributes: s.Attributes})
			}
		case "result":
			if msg.ID == statesID {
				var states []state
				if err := json.Unmarshal(msg.Result, &states); err != nil {
					return fmt.Errorf("get_states: %w", err)
				}
				for _, s := range states {
					if watched[s.EntityID] {
						update(backend.Entity{ID: s.EntityID, State: s.State, Attributes: s.Attributes})
					}
				}
				continue
			}
			b.mu.Lock()
			ch := b.pending[msg.ID]
			delete(b.pending, msg.ID)
			b.mu.Unlock()
			if ch != nil {
				ch <- msg.result
			}
		}
	}
}

func authenticate(conn *websocket.Conn, token string) error {
	var msg struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	}
	if err := conn.ReadJSON(&msg); err != nil {
		return err
	}
	if msg.Type != "auth_required" {
		return fmt.Errorf("unexpected greeting %q", msg.Type)
	}
	if err := conn.WriteJSON(map[string]any{"type": "auth", "access_token": token}); err != nil {
		return err
	}
	if err := conn.ReadJSON(&msg); err != nil {
		return err
	}
	if msg.Type != "auth_ok" {
		return fmt.Errorf("authentication failed: %s %s", msg.Type, msg.Message)
	}
	return nil
}

// send writes a command with a fresh ID and returns that ID.
func (b *Backend) send(cmd map[string]any) (int, error) {
	id, _, err := b.sendWait(cmd, false)
	return id, err
}

func (b *Backend) sendWait(cmd map[string]any, wait bool) (int, chan result, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conn == nil {
		return 0, nil, errors.New("not connected to home assistant")
	}
	id := b.nextID
	b.nextID++
	cmd["id"] = id
	var ch chan result
	if wait {
		ch = make(chan result, 1)
		b.pending[id] = ch
	}
	if err := b.conn.WriteJSON(cmd); err != nil {
		delete(b.pending, id)
		return 0, nil, err
	}
	return id, ch, nil
}

func (b *Backend) CallService(ctx context.Context, domain, service, entityID string, data map[string]any) error {
	_, ch, err := b.sendWait(map[string]any{
		"type":         "call_service",
		"domain":       domain,
		"service":      service,
		"service_data": data,
		"target":       map[string]any{"entity_id": entityID},
	}, true)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	select {
	case res, ok := <-ch:
		if !ok {
			return errors.New("home assistant connection closed before the call finished")
		}
		if !res.Success {
			if res.Error != nil {
				return fmt.Errorf("%s.%s: %s", domain, service, res.Error.Message)
			}
			return fmt.Errorf("%s.%s failed", domain, service)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
