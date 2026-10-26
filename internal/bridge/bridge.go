// Package bridge translates between the TRÅDFRI gateway's resources and Home Assistant
// entities. It renders devices, groups and moods as the JSON the app expects, and turns
// the app's writes into service calls. Only light, plug and blind control is writable;
// everything else (names, groups, moods, remotes) is read-only.
package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/stanmart/overgang/internal/backend"
	"github.com/stanmart/overgang/internal/config"
	"github.com/stanmart/overgang/internal/state"
	t "github.com/stanmart/overgang/internal/tradfri"
)

var (
	ErrNotFound   = errors.New("not found")
	ErrReadOnly   = errors.New("read-only")
	ErrBadRequest = errors.New("bad request")
)

type Kind int

const (
	KindLight Kind = iota
	KindPlug
	KindBlind
	KindRemote
)

type Device struct {
	ID     int
	Entity string
	Kind   Kind
	name   string
	model  string
	group  *Group
}

type Group struct {
	ID      int
	Name    string
	entity  string
	members []*Device
	// The SuperGroup is a hidden group of every light. Its moods are what the app shows
	// as scenes; ordinary rooms have no moods on current firmware.
	super  bool
	scenes []*Scene
}

// Scene is a mood of the SuperGroup. Apart from the built-in "All off", each stands
// for a Home Assistant scene entity.
type Scene struct {
	ID     int
	Name   string
	entity string
	allOff bool
}

// Mood types (AttrMoodPredefined) seen on the real gateway.
const (
	moodUserScene = 2
	moodAllOff    = 3
)

type Bridge struct {
	backend backend.Backend
	created int64

	// Fixed after New.
	devices       []*Device
	deviceByID    map[int]*Device
	deviceByEnt   map[string]*Device
	groups        []*Group
	groupByID     map[int]*Group
	groupByEntity map[string]*Group

	mu         sync.Mutex
	entities   map[string]backend.Entity
	lastSeen   map[string]int64
	dimmer     map[string]int // last brightness while on, reported while off
	mireds     map[string]int // likewise for colour temperature
	superGroup *Group
	scenes     []string // scene entities to watch

	activeMood map[int]int
	notify     func(paths ...string)
}

// New lays out devices, groups and scenes from the config, taking stable IDs from the
// store.
func New(rooms []config.Room, scenes []config.Scene, b backend.Backend, ids *state.Store) (*Bridge, error) {
	br := &Bridge{
		backend:       b,
		created:       time.Now().Unix(),
		deviceByID:    map[int]*Device{},
		deviceByEnt:   map[string]*Device{},
		groupByID:     map[int]*Group{},
		groupByEntity: map[string]*Group{},
		entities:      map[string]backend.Entity{},
		lastSeen:      map[string]int64{},
		dimmer:        map[string]int{},
		mireds:        map[string]int{},
		activeMood:    map[int]int{},
		notify:        func(...string) {},
	}
	for _, r := range splitBlinds(rooms) {
		gid, err := ids.ID("group", r.Name, t.GroupIDBase)
		if err != nil {
			return nil, err
		}
		g := &Group{ID: gid, Name: r.Name, entity: r.Entity}
		for _, dc := range r.Devices {
			kind, err := kindOf(dc)
			if err != nil {
				return nil, err
			}
			did, err := ids.ID("device", dc.Entity, t.DeviceIDBase)
			if err != nil {
				return nil, err
			}
			d := &Device{ID: did, Entity: dc.Entity, Kind: kind, name: dc.Name, model: dc.Model, group: g}
			g.members = append(g.members, d)
			br.devices = append(br.devices, d)
			br.deviceByID[did] = d
			br.deviceByEnt[d.Entity] = d
		}
		if g.entity != "" {
			br.groupByEntity[g.entity] = g
		}
		br.groups = append(br.groups, g)
		br.groupByID[gid] = g
	}
	if err := br.addSuperGroup(scenes, ids); err != nil {
		return nil, err
	}
	return br, nil
}

func (b *Bridge) addSuperGroup(scenes []config.Scene, ids *state.Store) error {
	gid, err := ids.ID("group", "SuperGroup", t.GroupIDBase)
	if err != nil {
		return err
	}
	g := &Group{ID: gid, Name: "SuperGroup", super: true}
	for _, d := range b.devices {
		if d.Kind == KindLight {
			g.members = append(g.members, d)
		}
	}
	all := append([]config.Scene{{Name: "All off"}}, scenes...)
	for _, sc := range all {
		mid, err := ids.ID("mood", sc.Name, t.MoodIDBase)
		if err != nil {
			return err
		}
		g.scenes = append(g.scenes, &Scene{ID: mid, Name: sc.Name, entity: sc.Entity, allOff: sc.Entity == ""})
		if sc.Entity != "" {
			b.scenes = append(b.scenes, sc.Entity)
		}
	}
	b.activeMood[g.ID] = g.scenes[0].ID
	b.superGroup = g
	b.groups = append(b.groups, g)
	b.groupByID[gid] = g
	return nil
}

// splitBlinds moves the blinds of a room that also has lights or plugs into a group of
// their own. The app only offers room controls for a group of lights or a group of
// blinds, and asks the user to separate them otherwise.
func splitBlinds(rooms []config.Room) []config.Room {
	var out []config.Room
	for _, r := range rooms {
		var blinds, rest []config.Device
		for _, d := range r.Devices {
			if kind, err := kindOf(d); err == nil && kind == KindBlind {
				blinds = append(blinds, d)
			} else {
				rest = append(rest, d)
			}
		}
		if len(blinds) == 0 || !slices.ContainsFunc(rest, func(d config.Device) bool {
			kind, err := kindOf(d)
			return err == nil && (kind == KindLight || kind == KindPlug)
		}) {
			out = append(out, r)
			continue
		}
		lights := r
		lights.Devices = rest
		out = append(out, lights, config.Room{Name: r.Name + " blinds", Devices: blinds})
	}
	return out
}

func kindOf(d config.Device) (Kind, error) {
	if d.Kind == "remote" {
		return KindRemote, nil
	}
	if d.Kind != "" {
		return 0, fmt.Errorf("%s: unknown kind %q", d.Entity, d.Kind)
	}
	switch backend.Domain(d.Entity) {
	case "light":
		return KindLight, nil
	case "switch":
		return KindPlug, nil
	case "cover":
		return KindBlind, nil
	}
	return 0, fmt.Errorf("%s: can't show this domain; lights, switches and covers are supported, or set kind = \"remote\" on a battery sensor", d.Entity)
}

// OnChange sets the function told which resource paths changed, to notify observers.
func (b *Bridge) OnChange(f func(paths ...string)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.notify = f
}

// Watch lists the entities the backend must report on.
func (b *Bridge) Watch() []string {
	var out []string
	for _, d := range b.devices {
		out = append(out, d.Entity)
	}
	for _, g := range b.groups {
		if g.entity != "" {
			out = append(out, g.entity)
		}
	}
	return append(out, b.scenes...)
}

// Update takes a new entity state from the backend.
func (b *Bridge) Update(e backend.Entity) {
	b.mu.Lock()
	b.entities[e.ID] = e
	b.lastSeen[e.ID] = time.Now().Unix()
	if e.State == "on" {
		if v, ok := e.Float("brightness"); ok {
			b.dimmer[e.ID] = toDimmer(v)
		}
		if v, ok := e.Float("color_temp_kelvin"); ok && v > 0 {
			b.mireds[e.ID] = clampMireds(int(math.Round(1e6 / v)))
		}
	}
	var paths []string
	if d := b.deviceByEnt[e.ID]; d != nil {
		paths = append(paths, devicePath(d.ID), groupPath(d.group.ID))
	}
	if g := b.groupByEntity[e.ID]; g != nil {
		paths = append(paths, groupPath(g.ID))
	}
	if d := b.deviceByEnt[e.ID]; d != nil && d.Kind == KindLight {
		// Scenes show their lights' current settings, so they change too.
		paths = append(paths, groupPath(b.superGroup.ID))
		for _, sc := range b.superGroup.scenes {
			paths = append(paths, fmt.Sprintf("%s/%d/%d", t.PathMoods, b.superGroup.ID, sc.ID))
		}
	}
	notify := b.notify
	b.mu.Unlock()
	if len(paths) > 0 {
		notify(paths...)
	}
}

func devicePath(id int) string { return t.PathDevices + "/" + strconv.Itoa(id) }
func groupPath(id int) string  { return t.PathGroups + "/" + strconv.Itoa(id) }

// DeviceIDs lists every device, as GET /15001 returns them.
func (b *Bridge) DeviceIDs() []int {
	out := make([]int, 0, len(b.devices))
	for _, d := range b.devices {
		out = append(out, d.ID)
	}
	return out
}

// GroupIDs lists every group, as GET /15004 returns them.
func (b *Bridge) GroupIDs() []int {
	out := make([]int, 0, len(b.groups))
	for _, g := range b.groups {
		out = append(out, g.ID)
	}
	return out
}

// MoodIDs lists a group's moods, as GET /15005/<group> returns them.
func (b *Bridge) MoodIDs(groupID int) ([]int, bool) {
	g := b.groupByID[groupID]
	if g == nil {
		return nil, false
	}
	out := []int{}
	for _, sc := range g.scenes {
		out = append(out, sc.ID)
	}
	return out, true
}

// Device renders GET /15001/<id>.
func (b *Bridge) Device(id int) (map[string]any, bool) {
	d := b.deviceByID[id]
	if d == nil {
		return nil, false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	e, known := b.entities[d.Entity]
	c := capabilitiesOf(e)

	name := d.name
	if name == "" {
		name = e.String("friendly_name")
	}
	if name == "" {
		name = d.Entity
	}
	info := map[string]any{
		t.InfoManufacturer: "IKEA of Sweden",
		t.InfoModel:        d.modelFor(c),
		t.InfoSerial:       "",
		t.InfoFirmware:     "2.3.095",
		t.InfoPowerSource:  t.PowerInternal,
		t.InfoImageType:    0,
		t.InfoProfile:      0,
	}
	out := map[string]any{
		t.AttrName:           name,
		t.AttrCreatedAt:      b.created,
		t.AttrID:             d.ID,
		t.AttrReachable:      boolInt(known && e.Available()),
		t.AttrLastSeen:       b.lastSeen[d.Entity],
		t.AttrOTAUpdateState: 0,
		t.AttrDeviceInfo:     info,
	}
	switch d.Kind {
	case KindLight:
		out[t.AttrDeviceType] = t.TypeLight
		out[t.AttrLightControl] = []any{b.lightState(d.Entity, e, c)}
		// Values the real gateway reports for its WS and CWS bulbs.
		switch {
		case c.color:
			info[t.InfoImageType], info[t.InfoProfile] = 10243, 3
		case c.colorTemp:
			info[t.InfoImageType], info[t.InfoProfile] = 16900, 5
		}
	case KindPlug:
		out[t.AttrDeviceType] = t.TypePlug
		out[t.AttrPlugControl] = []any{map[string]any{t.AttrOnOff: boolInt(e.State == "on"), t.AttrUnknown5849: 0, t.AttrDimmer: t.MaxDimmer, t.AttrID: 0}}
		info[t.InfoImageType] = 4353
	case KindBlind:
		out[t.AttrDeviceType] = t.TypeBlind
		pos, _ := e.Float("current_position")
		out[t.AttrBlindControl] = []any{map[string]any{t.AttrBlindPosition: 100 - int(math.Round(pos)), t.AttrID: 0}}
	case KindRemote:
		out[t.AttrDeviceType] = t.TypeRemote
		out[t.AttrSwitch] = []any{map[string]any{t.AttrID: 0}}
		info[t.InfoPowerSource] = t.PowerBattery
		if v, err := strconv.ParseFloat(e.State, 64); err == nil {
			info[t.InfoBattery] = int(math.Round(v))
		}
	}
	return out, true
}

// lightState renders one light's 3311 entry. Home Assistant drops brightness and colour
// while a light is off, but the app shows them anyway, so the last known values stand in.
func (b *Bridge) lightState(entityID string, e backend.Entity, c capabilities) map[string]any {
	on := e.State == "on"
	dimmer, ok := b.dimmer[entityID]
	if !ok {
		dimmer = t.MaxDimmer
	}
	out := map[string]any{t.AttrOnOff: boolInt(on), t.AttrUnknown5849: 2, t.AttrDimmer: dimmer, t.AttrID: 0}
	if c.colorTemp {
		out[t.AttrUnknown5717] = 0
		mireds, ok := b.mireds[entityID]
		if !ok {
			mireds = 370
		}
		out[t.AttrColorTemp] = mireds
		out[t.AttrColorHex] = t.NearestPreset(mireds)
	}
	if c.color && on {
		if xy, ok := e.Floats("xy_color"); ok && len(xy) == 2 {
			out[t.AttrColorX] = int(math.Round(xy[0] * t.MaxColor))
			out[t.AttrColorY] = int(math.Round(xy[1] * t.MaxColor))
		}
		if hs, ok := e.Floats("hs_color"); ok && len(hs) == 2 {
			out[t.AttrHue] = int(math.Round(hs[0] / 360 * t.MaxColor))
			out[t.AttrSaturation] = int(math.Round(hs[1] / 100 * t.MaxSaturation))
		}
		if rgb, ok := e.Floats("rgb_color"); ok && len(rgb) == 3 {
			out[t.AttrColorHex] = t.RGBToHex(rgb[0], rgb[1], rgb[2])
		}
	}
	return out
}

// Group renders GET /15004/<id>.
func (b *Bridge) Group(id int) (map[string]any, bool) {
	g := b.groupByID[id]
	if g == nil {
		return nil, false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	on, dimmer := false, 0
	if e, ok := b.entities[g.entity]; ok {
		on = e.State == "on"
		dimmer = b.dimmer[g.entity]
	} else {
		for _, d := range g.members {
			if d.Kind == KindLight && b.entities[d.Entity].State == "on" {
				on = true
				dimmer = max(dimmer, b.dimmer[d.Entity])
			}
		}
	}
	members := []int{}
	for _, d := range g.members {
		members = append(members, d.ID)
	}
	groupType := 0
	if g.super {
		groupType = 2
		dimmer = 0
	}
	return map[string]any{
		t.AttrName:      g.Name,
		t.AttrCreatedAt: b.created,
		t.AttrID:        g.ID,
		t.AttrOnOff:     boolInt(on),
		t.AttrDimmer:    dimmer,
		t.AttrGroupMood: b.activeMood[g.ID],
		t.AttrGroupType: groupType,
		t.AttrGroupMembers: map[string]any{
			t.AttrHSLink: map[string]any{t.AttrID: members},
		},
	}, true
}

// Mood renders GET /15005/<group>/<mood>: a scene, as only the SuperGroup has moods.
// "All off" lists every light and plug as off. A Home Assistant scene lists the lights
// it contains as on, with their current settings: Home Assistant doesn't expose a
// scene's settings, and the app waits after activating a scene until every light
// matches it, as on the real gateway.
func (b *Bridge) Mood(groupID, moodID int) (map[string]any, bool) {
	g := b.groupByID[groupID]
	if g == nil {
		return nil, false
	}
	sc, index := g.scene(moodID)
	if sc == nil {
		return nil, false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	inScene := map[string]bool{}
	for _, id := range b.entities[sc.entity].Strings("entity_id") {
		inScene[id] = true
	}

	lights, plugs := []any{}, []any{}
	for _, d := range b.devices {
		switch {
		case sc.allOff && d.Kind == KindLight:
			lights = append(lights, map[string]any{t.AttrOnOff: 0, t.AttrID: d.ID})
		case sc.allOff && d.Kind == KindPlug:
			plugs = append(plugs, map[string]any{t.AttrOnOff: 0, t.AttrID: d.ID})
		case inScene[d.Entity] && d.Kind == KindLight:
			e := b.entities[d.Entity]
			l := b.lightState(d.Entity, e, capabilitiesOf(e))
			l[t.AttrOnOff], l[t.AttrID] = 1, d.ID
			delete(l, t.AttrUnknown5849)
			delete(l, t.AttrUnknown5717)
			lights = append(lights, l)
		case inScene[d.Entity] && d.Kind == KindPlug:
			plugs = append(plugs, map[string]any{t.AttrOnOff: 1, t.AttrID: d.ID})
		}
	}
	kind, icon := moodUserScene, 2
	if sc.allOff {
		kind, icon = moodAllOff, 0
	}
	return map[string]any{
		t.AttrName:           sc.Name,
		t.AttrSceneIcon:      icon,
		t.AttrCreatedAt:      b.created,
		t.AttrID:             sc.ID,
		t.AttrSceneIndex:     index,
		t.AttrMoodPredefined: kind,
		t.AttrMoodLights:     lights,
		t.AttrMoodPlugs:      plugs,
	}, true
}

func (g *Group) scene(id int) (*Scene, int) {
	for i, sc := range g.scenes {
		if sc.ID == id {
			return sc, i
		}
	}
	return nil, 0
}

// SetDevice handles PUT /15001/<id>.
func (b *Bridge) SetDevice(ctx context.Context, id int, body map[string]any) error {
	d := b.deviceByID[id]
	if d == nil {
		return ErrNotFound
	}
	switch {
	case d.Kind == KindLight && body[t.AttrLightControl] != nil:
		s, err := first(body[t.AttrLightControl])
		if err != nil {
			return err
		}
		service, data, err := lightCommand(s)
		if err != nil {
			return err
		}
		return b.call(ctx, "light", service, d.Entity, data)
	case d.Kind == KindPlug && body[t.AttrPlugControl] != nil:
		s, err := first(body[t.AttrPlugControl])
		if err != nil {
			return err
		}
		on, ok := intVal(s, t.AttrOnOff)
		if !ok {
			return ErrReadOnly
		}
		return b.call(ctx, backend.Domain(d.Entity), onOffService(on != 0), d.Entity, nil)
	case d.Kind == KindBlind && body[t.AttrBlindControl] != nil:
		s, err := first(body[t.AttrBlindControl])
		if err != nil {
			return err
		}
		if _, ok := s[t.AttrBlindTrigger]; ok {
			return b.call(ctx, "cover", "stop_cover", d.Entity, nil)
		}
		pos, ok := intVal(s, t.AttrBlindPosition)
		if !ok {
			return ErrReadOnly
		}
		return b.call(ctx, "cover", "set_cover_position", d.Entity, map[string]any{"position": 100 - clamp(pos, 0, 100)})
	}
	return ErrReadOnly
}

// SetGroup handles PUT /15004/<id>: switching, dimming or setting a mood on a room.
func (b *Bridge) SetGroup(ctx context.Context, id int, body map[string]any) error {
	g := b.groupByID[id]
	if g == nil {
		return ErrNotFound
	}
	if moodID, ok := intVal(body, t.AttrGroupMood); ok {
		sc, _ := g.scene(moodID)
		if sc == nil {
			return ErrNotFound
		}
		b.mu.Lock()
		b.activeMood[g.ID] = sc.ID
		notify := b.notify
		b.mu.Unlock()
		notify(groupPath(g.ID))
		return b.applyScene(ctx, sc)
	}
	service, data, err := lightCommand(body)
	if err != nil {
		return err
	}
	if len(data) == 0 && body[t.AttrOnOff] == nil {
		return ErrReadOnly
	}
	if g.entity != "" {
		return b.call(ctx, "light", service, g.entity, data)
	}
	var errs []error
	for _, d := range g.members {
		switch d.Kind {
		case KindLight:
			errs = append(errs, b.call(ctx, "light", service, d.Entity, data))
		case KindPlug:
			// Plugs follow the room's on/off, as on the real gateway, but ignore dimming.
			if _, ok := body[t.AttrOnOff]; ok {
				errs = append(errs, b.call(ctx, backend.Domain(d.Entity), service, d.Entity, nil))
			}
		}
	}
	return errors.Join(errs...)
}

func (b *Bridge) applyScene(ctx context.Context, sc *Scene) error {
	if !sc.allOff {
		return b.call(ctx, "scene", "turn_on", sc.entity, nil)
	}
	var errs []error
	for _, d := range b.devices {
		switch d.Kind {
		case KindLight:
			errs = append(errs, b.call(ctx, "light", "turn_off", d.Entity, nil))
		case KindPlug:
			errs = append(errs, b.call(ctx, backend.Domain(d.Entity), "turn_off", d.Entity, nil))
		}
	}
	return errors.Join(errs...)
}

func (b *Bridge) call(ctx context.Context, domain, service, entityID string, data map[string]any) error {
	if data == nil {
		data = map[string]any{}
	}
	slog.Debug("service call", "service", domain+"."+service, "entity", entityID, "data", data)
	return b.backend.CallService(ctx, domain, service, entityID, data)
}

// lightCommand turns the app's light settings into a light.turn_on or turn_off call.
// Every value is clamped to its range first: they come from the network.
func lightCommand(s map[string]any) (string, map[string]any, error) {
	data := map[string]any{}
	if tr, ok := intVal(s, t.AttrTransition); ok {
		data["transition"] = float64(clamp(tr, 0, maxTransition)) / 10
	}
	if on, ok := intVal(s, t.AttrOnOff); ok && on == 0 {
		return "turn_off", data, nil
	}
	if dim, ok := intVal(s, t.AttrDimmer); ok {
		if dim <= 0 {
			return "turn_off", data, nil
		}
		data["brightness"] = fromDimmer(min(dim, t.MaxDimmer))
	}
	hex, hasHex := s[t.AttrColorHex].(string)
	x, hasX := intVal(s, t.AttrColorX)
	y, hasY := intVal(s, t.AttrColorY)
	hue, hasHue := intVal(s, t.AttrHue)
	sat, hasSat := intVal(s, t.AttrSaturation)
	if mireds, ok := intVal(s, t.AttrColorTemp); ok && mireds > 0 {
		mireds = clamp(mireds, minAcceptedMireds, maxAcceptedMireds)
		data["color_temp_kelvin"] = int(math.Round(1e6 / float64(mireds)))
	} else if hasHex {
		if mireds, ok := t.PresetMireds(hex); ok {
			data["color_temp_kelvin"] = int(math.Round(1e6 / float64(mireds)))
		} else {
			x, y, err := t.HexToXY(hex)
			if err != nil {
				return "", nil, fmt.Errorf("%w: %v", ErrBadRequest, err)
			}
			data["xy_color"] = []float64{round4(x), round4(y)}
		}
	} else if hasX && hasY {
		x, y = clamp(x, 0, t.MaxColor), clamp(y, 0, t.MaxColor)
		data["xy_color"] = []float64{round4(float64(x) / t.MaxColor), round4(float64(y) / t.MaxColor)}
	} else if hasHue && hasSat {
		hue, sat = clamp(hue, 0, t.MaxColor), clamp(sat, 0, t.MaxSaturation)
		data["hs_color"] = []float64{float64(hue) / t.MaxColor * 360, float64(sat) / t.MaxSaturation * 100}
	}
	return "turn_on", data, nil
}

type capabilities struct{ colorTemp, color bool }

func capabilitiesOf(e backend.Entity) capabilities {
	var c capabilities
	for _, m := range e.Strings("supported_color_modes") {
		switch m {
		case "color_temp":
			c.colorTemp = true
		case "hs", "xy", "rgb", "rgbw", "rgbww":
			c.color = true
		}
	}
	return c
}

func (d *Device) modelFor(c capabilities) string {
	if d.model != "" {
		return d.model
	}
	switch d.Kind {
	case KindPlug:
		return "TRADFRI control outlet"
	case KindBlind:
		return "FYRTUR block-out roller blind"
	case KindRemote:
		return "TRADFRI remote control"
	}
	switch {
	case c.color:
		return "TRADFRI bulb E27 CWS opal 600lm"
	case c.colorTemp:
		return "TRADFRI bulb E27 WS opal 980lm"
	}
	return "TRADFRI bulb E27 W opal 1000lm"
}

func first(v any) (map[string]any, error) {
	list, ok := v.([]any)
	if !ok || len(list) == 0 {
		return nil, fmt.Errorf("%w: expected a one-element list", ErrBadRequest)
	}
	m, ok := list[0].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%w: expected an object", ErrBadRequest)
	}
	return m, nil
}

// intVal reads an integer from a decoded JSON object. Both json.Number (from a
// UseNumber decoder) and float64 are accepted. The app sends some integers with a
// fraction, such as a blind position of 73.0, so those are rounded.
func intVal(m map[string]any, key string) (int, bool) {
	switch v := m[key].(type) {
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return int(n), true
		}
		f, err := v.Float64()
		return int(math.Round(f)), err == nil
	case float64:
		return int(v), true
	case int:
		return v, true
	}
	return 0, false
}

func onOffService(on bool) string {
	if on {
		return "turn_on"
	}
	return "turn_off"
}

func toDimmer(brightness float64) int {
	return int(math.Round(brightness * t.MaxDimmer / 255))
}

func fromDimmer(dimmer int) int {
	return int(math.Round(float64(dimmer) * 255 / t.MaxDimmer))
}

// Limits on what the app may ask for. The transition is in tenths of a second, so ten
// minutes; colour temperatures cover 1000–10000 K, wider than any bulb.
const (
	maxTransition     = 6000
	minAcceptedMireds = 100
	maxAcceptedMireds = 1000
)

func clamp(v, lo, hi int) int {
	return max(lo, min(hi, v))
}

func clampMireds(m int) int {
	return max(t.MinMireds, min(t.MaxMireds, m))
}

func round4(v float64) float64 {
	return math.Round(v*1e4) / 1e4
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
