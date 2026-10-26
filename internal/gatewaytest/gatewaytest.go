// Package gatewaytest runs an emulated gateway with the fake backend for tests.
package gatewaytest

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stanmart/overgang/internal/backend/fake"
	"github.com/stanmart/overgang/internal/bridge"
	"github.com/stanmart/overgang/internal/config"
	"github.com/stanmart/overgang/internal/gateway"
	"github.com/stanmart/overgang/internal/state"
)

// Start serves rooms and scenes on a free loopback port until the test ends. It returns
// the address and the state store, which also holds the assigned IDs.
func Start(tb testing.TB, rooms []config.Room, scenes []config.Scene, securityCode string) (string, *state.Store) {
	tb.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	tb.Cleanup(cancel)

	store, err := state.Open(filepath.Join(tb.TempDir(), "state.json"))
	if err != nil {
		tb.Fatal(err)
	}
	modes := map[string][]string{}
	for _, r := range rooms {
		for _, d := range r.Devices {
			modes[d.Entity] = d.FakeColorModes
		}
	}
	members := map[string][]string{}
	for _, s := range scenes {
		members[s.Entity] = s.FakeMembers
	}
	be := fake.New(modes, members)
	br, err := bridge.New(rooms, scenes, be, store)
	if err != nil {
		tb.Fatal(err)
	}
	gw := gateway.New(gateway.Config{
		Listen:       "127.0.0.1:0",
		SecurityCode: securityCode,
		Firmware:     "1.21.57",
		GatewayID:    "020000004f47",
		Resources:    map[string]json.RawMessage{"test/extra": json.RawMessage(`[]`)},
	}, br, store)
	ready := make(chan struct{})
	go be.Run(ctx, br.Watch(), br.Update)
	go func() {
		if err := gw.Run(ctx, ready); err != nil {
			tb.Error(err)
		}
	}()
	<-ready
	return gw.Addr(), store
}
