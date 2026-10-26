// Package backend defines what ÖVERGÅNG needs from whatever owns the real devices. The
// model is Home Assistant's: entities with a state string and attributes, changed by
// calling services. The fake backend implements the same model in memory.
package backend

import (
	"context"
	"strings"
)

// Entity is one Home Assistant entity's current state.
type Entity struct {
	ID         string
	State      string
	Attributes map[string]any
}

// Backend owns the real devices.
type Backend interface {
	// Run keeps the backend connected. It calls update with the current state of every
	// watched entity, then again whenever one changes, and returns when ctx is done.
	Run(ctx context.Context, watch []string, update func(Entity)) error
	// CallService calls a service such as light.turn_on on one entity.
	CallService(ctx context.Context, domain, service, entityID string, data map[string]any) error
}

// Domain returns the part of an entity ID before the dot: "light" for "light.desk".
func Domain(entityID string) string {
	d, _, _ := strings.Cut(entityID, ".")
	return d
}

// Float reads a numeric attribute. JSON numbers decode as float64.
func (e Entity) Float(key string) (float64, bool) {
	switch v := e.Attributes[key].(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	}
	return 0, false
}

// Floats reads a numeric list attribute such as xy_color.
func (e Entity) Floats(key string) ([]float64, bool) {
	var out []float64
	switch v := e.Attributes[key].(type) {
	case []any:
		for _, x := range v {
			f, ok := x.(float64)
			if !ok {
				return nil, false
			}
			out = append(out, f)
		}
	case []float64:
		out = v
	default:
		return nil, false
	}
	return out, true
}

// Strings reads a string list attribute such as supported_color_modes.
func (e Entity) Strings(key string) []string {
	var out []string
	switch v := e.Attributes[key].(type) {
	case []any:
		for _, x := range v {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
	case []string:
		out = v
	}
	return out
}

// String reads a string attribute.
func (e Entity) String(key string) string {
	s, _ := e.Attributes[key].(string)
	return s
}

// Available reports whether Home Assistant can reach the device.
func (e Entity) Available() bool {
	return e.State != "" && e.State != "unavailable" && e.State != "unknown"
}
