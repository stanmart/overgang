package proxy_test

import (
	"context"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stanmart/overgang/internal/client"
	"github.com/stanmart/overgang/internal/config"
	"github.com/stanmart/overgang/internal/gatewaytest"
	"github.com/stanmart/overgang/internal/proxy"
	"github.com/stanmart/overgang/internal/state"
	t "github.com/stanmart/overgang/internal/tradfri"
)

// The emulator stands in for the real gateway: the app pairs through the proxy, which
// must learn the new key to relay the app's next session.
func TestProxyLearnsPairingAndRelays(tt *testing.T) {
	const code = "real-gateway-code"
	gw, _ := gatewaytest.Start(tt, []config.Room{{Name: "Room", Devices: []config.Device{{Entity: "light.one"}}}}, code)

	store, err := state.Open(filepath.Join(tt.TempDir(), "proxy.json"))
	if err != nil {
		tt.Fatal(err)
	}
	// Grab a free port for the proxy.
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		tt.Fatal(err)
	}
	listen := pc.LocalAddr().String()
	pc.Close()

	var mu sync.Mutex
	var records []proxy.Record
	p := &proxy.Proxy{Listen: listen, Gateway: gw, SecurityCode: code, Store: store, Capture: func(r proxy.Record) {
		mu.Lock()
		records = append(records, r)
		mu.Unlock()
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go p.Run(ctx)
	time.Sleep(100 * time.Millisecond)

	psk, err := client.Pair(ctx, listen, code, "app")
	if err != nil {
		tt.Fatal(err)
	}
	if learned, _ := store.PSK("app"); learned != psk {
		tt.Fatalf("proxy learned %q, app got %q", learned, psk)
	}
	c, err := client.Dial(ctx, listen, "app", psk)
	if err != nil {
		tt.Fatal(err)
	}
	defer c.Close()
	b, err := c.Get(ctx, t.PathDevices)
	if err != nil {
		tt.Fatal(err)
	}
	if !strings.HasPrefix(string(b), "[65536") {
		tt.Errorf("devices through the proxy: %s", b)
	}

	mu.Lock()
	defer mu.Unlock()
	var sawRequest, sawResponse bool
	for _, r := range records {
		sawRequest = sawRequest || (r.Direction == "app→gw" && r.Path == t.PathDevices)
		sawResponse = sawResponse || (r.Direction == "gw→app" && strings.HasPrefix(string(r.Payload), "[65536"))
	}
	if !sawRequest || !sawResponse {
		tt.Errorf("capture missed the exchange: %+v", records)
	}
}
