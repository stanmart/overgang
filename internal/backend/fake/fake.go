// Package fake is an in-memory backend that makes up a device for every entity it is
// asked to watch, so the gateway can be tried without Home Assistant. It follows Home
// Assistant's conventions closely enough to exercise the real mapping code: lights drop
// their brightness and colour attributes when off, covers report 0–100 with 100 open.
package fake

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/stanmart/overgang/internal/backend"
)

// Backend is the fake. The zero value is not usable; call New.
type Backend struct {
	colorModes   map[string][]string
	sceneMembers map[string][]string

	mu       sync.Mutex
	entities map[string]*entity
	update   func(backend.Entity)
}

type entity struct {
	id       string
	on       bool
	bri      float64
	kelvin   float64
	xy       []float64
	mode     string
	modes    []string
	position float64
	battery  float64
	members  []string // for scenes
}

// New makes a fake backend. colorModes sets supported_color_modes per light entity;
// lights not listed are colour-temperature lights. sceneMembers lists the entities each
// scene turns on.
func New(colorModes, sceneMembers map[string][]string) *Backend {
	return &Backend{colorModes: colorModes, sceneMembers: sceneMembers, entities: map[string]*entity{}}
}

func (b *Backend) Run(ctx context.Context, watch []string, update func(backend.Entity)) error {
	b.mu.Lock()
	b.update = update
	var initial []backend.Entity
	for _, id := range watch {
		e := &entity{id: id, bri: 200, kelvin: 3000, xy: []float64{0.45, 0.41}, position: 100, battery: 87}
		e.modes = b.colorModes[id]
		if len(e.modes) == 0 {
			e.modes = []string{"color_temp"}
		}
		e.mode = e.modes[0]
		e.members = b.sceneMembers[id]
		b.entities[id] = e
		initial = append(initial, e.render())
	}
	b.mu.Unlock()

	for _, e := range initial {
		update(e)
	}
	<-ctx.Done()
	return nil
}

func (b *Backend) CallService(_ context.Context, domain, service, entityID string, data map[string]any) error {
	b.mu.Lock()
	e, ok := b.entities[entityID]
	if !ok {
		b.mu.Unlock()
		return fmt.Errorf("fake: unknown entity %s", entityID)
	}
	changed := []*entity{e}
	if domain == "scene" && service == "turn_on" {
		// A scene turns its members on; the scene entity itself doesn't change.
		changed = nil
		for _, id := range e.members {
			if m := b.entities[id]; m != nil {
				m.on = true
				changed = append(changed, m)
			}
		}
	} else if err := e.apply(domain+"."+service, data); err != nil {
		b.mu.Unlock()
		return err
	}
	var states []backend.Entity
	for _, c := range changed {
		states = append(states, c.render())
	}
	update := b.update
	b.mu.Unlock()

	slog.Info("fake service call", "service", domain+"."+service, "entity", entityID, "data", data)
	for _, s := range states {
		update(s)
	}
	return nil
}

func (e *entity) apply(service string, data map[string]any) error {
	switch service {
	case "light.turn_on":
		e.on = true
		if v, ok := num(data["brightness"]); ok {
			e.bri = v
		}
		if v, ok := num(data["color_temp_kelvin"]); ok {
			e.kelvin, e.mode = v, "color_temp"
		}
		if v, ok := data["xy_color"].([]float64); ok {
			e.xy, e.mode = v, "xy"
		}
	case "light.turn_off", "switch.turn_off":
		e.on = false
	case "switch.turn_on":
		e.on = true
	case "cover.set_cover_position":
		if v, ok := num(data["position"]); ok {
			e.position = v
		}
	case "cover.open_cover":
		e.position = 100
	case "cover.close_cover":
		e.position = 0
	case "cover.stop_cover":
	default:
		return fmt.Errorf("fake: unsupported service %s", service)
	}
	return nil
}

func (e *entity) render() backend.Entity {
	attrs := map[string]any{"friendly_name": friendlyName(e.id)}
	out := backend.Entity{ID: e.id, Attributes: attrs}
	switch backend.Domain(e.id) {
	case "light":
		out.State = onOff(e.on)
		attrs["supported_color_modes"] = e.modes
		if e.on {
			attrs["color_mode"] = e.mode
			attrs["brightness"] = e.bri
			if e.mode == "color_temp" {
				attrs["color_temp_kelvin"] = e.kelvin
			} else {
				attrs["xy_color"] = e.xy
			}
		}
	case "switch":
		out.State = onOff(e.on)
	case "cover":
		out.State = "open"
		if e.position == 0 {
			out.State = "closed"
		}
		attrs["current_position"] = e.position
	case "scene":
		out.State = "2026-01-01T00:00:00+00:00"
		attrs["entity_id"] = e.members
	case "sensor":
		out.State = fmt.Sprint(e.battery)
		attrs["unit_of_measurement"] = "%"
		attrs["device_class"] = "battery"
	default:
		out.State = "unknown"
	}
	return out
}

func friendlyName(id string) string {
	_, name, _ := strings.Cut(id, ".")
	name = strings.ReplaceAll(name, "_", " ")
	if name == "" {
		return id
	}
	return strings.ToUpper(name[:1]) + name[1:]
}

func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

func num(v any) (float64, bool) {
	switch v := v.(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	}
	return 0, false
}
