package gateway_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"testing"
	"time"

	piondtls "github.com/pion/dtls/v3"
	"github.com/plgd-dev/go-coap/v3/message"
	"github.com/plgd-dev/go-coap/v3/message/codes"
	"github.com/plgd-dev/go-coap/v3/udp/coder"

	"github.com/stanmart/overgang/internal/client"
	"github.com/stanmart/overgang/internal/config"
	"github.com/stanmart/overgang/internal/gatewaytest"
	t "github.com/stanmart/overgang/internal/tradfri"
)

const code = "test-security-code"

var rooms = []config.Room{
	{Name: "Bedroom", Devices: []config.Device{
		{Entity: "light.ceiling"},
		{Entity: "light.desk", Name: "Desk lamp"},
		{Entity: "cover.blind"},
		{Entity: "sensor.remote_battery", Kind: "remote"},
	}},
	{Name: "Dining", Devices: []config.Device{
		{Entity: "light.colour", FakeColorModes: []string{"xy"}},
		{Entity: "switch.tree"},
	}},
}

var scenes = []config.Scene{
	{Name: "Evening", Entity: "scene.evening", FakeMembers: []string{"light.ceiling", "switch.tree"}},
}

// start runs the gateway on a free port with the fake backend.
func start(tb testing.TB) (addr string, ids map[string]int) {
	tb.Helper()
	addr, store := gatewaytest.Start(tb, rooms, scenes, code)
	ids = map[string]int{}
	for _, r := range rooms {
		for _, d := range r.Devices {
			ids[d.Entity], _ = store.ID("device", d.Entity, t.DeviceIDBase)
		}
	}
	return addr, ids
}

func connect(tb testing.TB, addr string) *client.Client {
	tb.Helper()
	ctx := context.Background()
	psk, err := client.Pair(ctx, addr, code, "test-app")
	if err != nil {
		tb.Fatal(err)
	}
	c, err := client.Dial(ctx, addr, "test-app", psk)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { c.Close() })
	return c
}

func getJSON(tb testing.TB, c *client.Client, path string) map[string]any {
	tb.Helper()
	b, err := c.Get(context.Background(), path)
	if err != nil {
		tb.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		tb.Fatalf("%s: %v: %s", path, err, b)
	}
	return out
}

func light(m map[string]any) map[string]any {
	return m[t.AttrLightControl].([]any)[0].(map[string]any)
}

func devicePath(id int) string { return fmt.Sprintf("%s/%d", t.PathDevices, id) }

func TestPairing(tt *testing.T) {
	addr, _ := start(tt)

	// A wrong key fails the handshake's Finished check, which the client only notices
	// by timing out.
	wrong, cancelWrong := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelWrong()
	if _, err := client.Pair(wrong, addr, "wrong code", "x"); err == nil {
		tt.Error("pairing with the wrong code worked")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	boot, err := client.Dial(ctx, addr, t.BootstrapIdentity, code)
	if err != nil {
		tt.Fatal(err)
	}
	defer boot.Close()
	if _, err := boot.Get(ctx, t.PathDevices); err == nil {
		tt.Error("the bootstrap identity could read devices")
	}
}

func TestDumpAndControl(tt *testing.T) {
	addr, ids := start(tt)
	c := connect(tt, addr)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	dump, err := c.Dump(ctx)
	if err != nil {
		tt.Fatal(err)
	}
	var devices []int
	json.Unmarshal(dump[t.PathDevices], &devices)
	if len(devices) != 6 {
		tt.Fatalf("got %d devices, want 6", len(devices))
	}

	desk := getJSON(tt, c, devicePath(ids["light.desk"]))
	if desk[t.AttrName] != "Desk lamp" || desk[t.AttrReachable] != 1.0 {
		tt.Errorf("desk lamp: %v", desk)
	}
	if model := desk[t.AttrDeviceInfo].(map[string]any)[t.InfoModel]; model != "TRADFRI bulb E27 WS opal 980lm" {
		tt.Errorf("desk lamp model %v", model)
	}

	// Observe the desk lamp, then dim and warm it the way the app does.
	notes := make(chan map[string]any, 8)
	go c.Observe(ctx, devicePath(ids["light.desk"]), func(b []byte) {
		var m map[string]any
		if json.Unmarshal(b, &m) == nil {
			notes <- m
		}
	})
	<-notes // the observe registration answers with the current state
	put := func(path, body string) error { return c.Put(ctx, path, []byte(body)) }
	if err := put(devicePath(ids["light.desk"]), `{"3311":[{"5850":1,"5851":127,"5711":400,"5712":5}]}`); err != nil {
		tt.Fatal(err)
	}
	select {
	case m := <-notes:
		l := light(m)
		if l[t.AttrOnOff] != 1.0 || l[t.AttrDimmer] != 127.0 || l[t.AttrColorTemp] != 400.0 {
			tt.Errorf("after dimming: %v", l)
		}
	case <-ctx.Done():
		tt.Fatal("no observe notification")
	}

	// A white preset hex is a colour temperature; another hex is a colour.
	if err := put(devicePath(ids["light.desk"]), `{"3311":[{"5706":"f5faf6"}]}`); err != nil {
		tt.Fatal(err)
	}
	if l := light(getJSON(tt, c, devicePath(ids["light.desk"]))); l[t.AttrColorTemp] != 250.0 {
		tt.Errorf("after cool preset: %v", l)
	}
	if err := put(devicePath(ids["light.colour"]), `{"3311":[{"5850":1,"5706":"e78834"}]}`); err != nil {
		tt.Fatal(err)
	}
	if l := light(getJSON(tt, c, devicePath(ids["light.colour"]))); l[t.AttrColorX] == nil {
		tt.Errorf("colour light has no xy after a hex colour: %v", l)
	}

	// The app writes gateway settings such as daylight saving rules; they are echoed back.
	if err := put(t.PathGateway+"/"+t.PathGatewayInfo, `{"9236":3600,"9072":3}`); err != nil {
		tt.Fatal(err)
	}
	if info := getJSON(tt, c, t.PathGateway+"/"+t.PathGatewayInfo); info["9236"] != 3600.0 || info[t.AttrFirmware] != "1.21.57" {
		tt.Errorf("gateway info after settings write: %v", info)
	}

	// Extra resources from the config are served, and writes replace them.
	if b, err := c.Get(ctx, "test/extra"); err != nil || string(b) != "[]" {
		tt.Errorf("extra resource: %s, %v", b, err)
	}
	if err := put("test/extra", `[1]`); err != nil {
		tt.Fatal(err)
	}
	if b, _ := c.Get(ctx, "test/extra"); string(b) != "[1]" {
		tt.Errorf("extra resource after write: %s", b)
	}

	// Renaming is read-only.
	if err := put(devicePath(ids["light.desk"]), `{"9001":"Renamed"}`); err == nil {
		tt.Error("rename was accepted")
	}

	// Blind: the app's 0 is open, Home Assistant's 100. The app sends positions with a
	// fraction.
	if err := put(devicePath(ids["cover.blind"]), `{"15015":[{"5536":30.0}]}`); err != nil {
		tt.Fatal(err)
	}
	blind := getJSON(tt, c, devicePath(ids["cover.blind"]))[t.AttrBlindControl].([]any)[0].(map[string]any)
	if blind[t.AttrBlindPosition] != 30.0 {
		tt.Errorf("blind: %v", blind)
	}

	// Plug.
	if err := put(devicePath(ids["switch.tree"]), `{"3312":[{"5850":1}]}`); err != nil {
		tt.Fatal(err)
	}

	// Remote battery comes from the sensor's state.
	remote := getJSON(tt, c, devicePath(ids["sensor.remote_battery"]))
	if remote[t.AttrDeviceType] != float64(t.TypeRemote) || remote[t.AttrDeviceInfo].(map[string]any)[t.InfoBattery] != 87.0 {
		tt.Errorf("remote: %v", remote)
	}

	// The bedroom's blind gets a group of its own, and the SuperGroup comes last.
	var groups []int
	json.Unmarshal(dump[t.PathGroups], &groups)
	if len(groups) != 4 {
		tt.Fatalf("got %d groups, want 4", len(groups))
	}
	if g := getJSON(tt, c, fmt.Sprintf("%s/%d", t.PathGroups, groups[1])); g[t.AttrName] != "Bedroom blinds" {
		tt.Errorf("second group: %v", g)
	}
	bedroom := fmt.Sprintf("%s/%d", t.PathGroups, groups[0])
	if err := put(bedroom, `{"5850":0}`); err != nil {
		tt.Fatal(err)
	}
	if g := getJSON(tt, c, bedroom); g[t.AttrOnOff] != 0.0 {
		tt.Errorf("bedroom still on: %v", g)
	}
	// Rooms have no moods; scenes are the SuperGroup's moods: "All off" first, then the
	// configured ones.
	if moods := string(dump[fmt.Sprintf("%s/%d", t.PathMoods, groups[0])]); moods != "[]" {
		tt.Errorf("bedroom moods: %s", moods)
	}
	super := getJSON(tt, c, fmt.Sprintf("%s/%d", t.PathGroups, groups[3]))
	if super[t.AttrGroupType] != 2.0 {
		tt.Errorf("SuperGroup: %v", super)
	}
	var sceneIDs []int
	json.Unmarshal(dump[fmt.Sprintf("%s/%d", t.PathMoods, groups[3])], &sceneIDs)
	if len(sceneIDs) != 2 {
		tt.Fatalf("scenes: %v", sceneIDs)
	}
	evening := getJSON(tt, c, fmt.Sprintf("%s/%d/%d", t.PathMoods, groups[3], sceneIDs[1]))
	if evening[t.AttrName] != "Evening" || evening[t.AttrMoodPredefined] != 2.0 || len(evening[t.AttrMoodLights].([]any)) != 1 || len(evening[t.AttrMoodPlugs].([]any)) != 1 {
		tt.Errorf("Evening scene: %v", evening)
	}
	// Lights in a scene carry full settings; the app waits for lights to match them.
	if l := evening[t.AttrMoodLights].([]any)[0].(map[string]any); l[t.AttrDimmer] == nil || l[t.AttrID] != float64(ids["light.ceiling"]) {
		tt.Errorf("Evening scene light: %v", l)
	}
	superPath := fmt.Sprintf("%s/%d", t.PathGroups, groups[3])
	if err := put(superPath, fmt.Sprintf(`{"9039":%d}`, sceneIDs[1])); err != nil {
		tt.Fatal(err)
	}
	if l := light(getJSON(tt, c, devicePath(ids["light.ceiling"]))); l[t.AttrOnOff] != 1.0 {
		tt.Errorf("ceiling after Evening: %v", l)
	}
	if err := put(superPath, fmt.Sprintf(`{"9039":%d}`, sceneIDs[0])); err != nil {
		tt.Fatal(err)
	}
	if l := light(getJSON(tt, c, devicePath(ids["light.ceiling"]))); l[t.AttrOnOff] != 0.0 {
		tt.Errorf("ceiling after All off: %v", l)
	}
}

// A peer that observes a light and then stops acknowledging must not hold up anyone
// else: notifications to it are confirmable, and used to block every state update
// until its connection timed out.
func TestSilentObserverDoesNotStallOthers(tt *testing.T) {
	addr, ids := start(tt)
	c := connect(tt, addr)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	silent := silentObserver(tt, addr, devicePath(ids["light.ceiling"]))
	defer silent.Close()

	for i, body := range []string{`{"3311":[{"5850":1}]}`, `{"3311":[{"5850":0}]}`, `{"3311":[{"5850":1}]}`} {
		start := time.Now()
		if err := c.Put(ctx, devicePath(ids["light.ceiling"]), []byte(body)); err != nil {
			tt.Fatalf("write %d: %v", i, err)
		}
		if d := time.Since(start); d > 2*time.Second {
			tt.Fatalf("write %d took %v with a silent observer around", i, d)
		}
	}
}

// silentObserver pairs, observes path with a raw CoAP request, reads the first answer
// and then never reads or acknowledges anything again.
func silentObserver(tb testing.TB, addr, path string) net.Conn {
	tb.Helper()
	ctx := context.Background()
	psk, err := client.Pair(ctx, addr, code, "silent")
	if err != nil {
		tb.Fatal(err)
	}
	raddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		tb.Fatal(err)
	}
	conn, err := piondtls.Dial("udp", raddr, &piondtls.Config{
		PSK:             func([]byte) ([]byte, error) { return []byte(psk), nil },
		PSKIdentityHint: []byte("silent"),
		CipherSuites:    []piondtls.CipherSuiteID{piondtls.TLS_PSK_WITH_AES_128_CCM_8},
	})
	if err != nil {
		tb.Fatal(err)
	}
	opts := make(message.Options, 0, 8)
	buf := make([]byte, 64)
	opts, _, _ = opts.SetPath(buf, "/"+path)
	opts, _, _ = opts.SetObserve(buf[32:], 0)
	req := message.Message{Token: message.Token("tok1"), Options: opts, Code: codes.GET, MessageID: 1, Type: message.Confirmable}
	out := make([]byte, 256)
	n, err := coder.DefaultCoder.Encode(req, out)
	if err != nil {
		tb.Fatal(err)
	}
	if _, err := conn.Write(out[:n]); err != nil {
		tb.Fatal(err)
	}
	if _, err := conn.Read(make([]byte, 2048)); err != nil {
		tb.Fatal(err)
	}
	return conn
}
