// Package config reads ÖVERGÅNG's TOML configuration.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/BurntSushi/toml"
)

type Config struct {
	Gateway Gateway `toml:"gateway"`
	Backend Backend `toml:"backend"`
	Rooms   []Room  `toml:"rooms"`
	Scenes  []Scene `toml:"scenes"`
}

type Gateway struct {
	// UDP address to serve CoAP over DTLS on. The real gateway uses port 5684.
	Listen string `toml:"listen"`
	// The code the app pairs with. OVERGANG_SECURITY_CODE overrides both.
	SecurityCode     string `toml:"security_code"`
	SecurityCodeFile string `toml:"security_code_file"`
	// Where app identities and assigned device IDs are kept.
	StateFile string `toml:"state_file"`
	// MAC-like ID: the gateway ID the app sees and the gw-<mac> mDNS name.
	MAC      string `toml:"mac"`
	Firmware string `toml:"firmware"`
	MDNS     bool   `toml:"mdns"`
	// Extra resources served as fixed JSON, keyed by path without a leading slash,
	// for trying out what the app expects at paths ÖVERGÅNG doesn't know.
	Resources map[string]string `toml:"resources"`
}

type Backend struct {
	// "fake" or "homeassistant".
	Kind string `toml:"kind"`
	// Home Assistant websocket URL, e.g. ws://127.0.0.1:8123/api/websocket.
	URL string `toml:"url"`
	// Long-lived access token. OVERGANG_HA_TOKEN overrides both.
	Token     string `toml:"token"`
	TokenFile string `toml:"token_file"`
}

// Room becomes a group in the app.
type Room struct {
	Name string `toml:"name"`
	// Optional light entity that controls the whole room at once, such as a Zigbee2MQTT
	// group. Without it, room commands go to each light separately.
	Entity  string   `toml:"entity"`
	Devices []Device `toml:"devices"`
}

// Device is one entity shown in the app. Its kind follows from the entity's domain:
// light, switch (shown as a plug) or cover (shown as a blind). A battery sensor with
// kind = "remote" is shown as a remote.
type Device struct {
	Entity string `toml:"entity"`
	Kind   string `toml:"kind"`
	// Shown name; defaults to the entity's friendly name.
	Name string `toml:"name"`
	// IKEA model string the app sees; defaults to one matching the device's features.
	Model string `toml:"model"`
	// Fake backend only: supported_color_modes for a made-up light.
	FakeColorModes []string `toml:"fake_color_modes"`
}

// Scene is a Home Assistant scene shown as a scene in the app, next to the built-in
// "All off". Activating it calls scene.turn_on.
type Scene struct {
	Name   string `toml:"name"`
	Entity string `toml:"entity"`
	// Fake backend only: the entities the made-up scene contains.
	FakeMembers []string `toml:"fake_members"`
}

// Load reads and checks a config file, applying defaults and environment overrides.
func Load(path string) (*Config, error) {
	cfg := Config{
		Gateway: Gateway{
			Listen:    ":5684",
			StateFile: "overgang-state.json",
			MAC:       "02:00:00:00:4f:47",
			Firmware:  "1.21.57",
			MDNS:      true,
		},
		Backend: Backend{Kind: "fake"},
	}
	md, err := toml.DecodeFile(path, &cfg)
	if err != nil {
		return nil, err
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		return nil, fmt.Errorf("%s: unknown keys %v", path, undecoded)
	}

	if cfg.Gateway.SecurityCode, err = secret("OVERGANG_SECURITY_CODE", cfg.Gateway.SecurityCode, cfg.Gateway.SecurityCodeFile); err != nil {
		return nil, err
	}
	if cfg.Gateway.SecurityCode == "" {
		return nil, errors.New("no security code: set gateway.security_code(_file) or OVERGANG_SECURITY_CODE")
	}
	// Anyone who has the code can pair and control the configured devices, and a
	// recorded pairing handshake lets an attacker test guesses offline. The real
	// gateway's codes are 16 random characters.
	if len(cfg.Gateway.SecurityCode) < 12 {
		return nil, errors.New("the security code must be at least 12 characters; use 16 random letters and digits like a real gateway")
	}
	if cfg.Backend.Token, err = secret("OVERGANG_HA_TOKEN", cfg.Backend.Token, cfg.Backend.TokenFile); err != nil {
		return nil, err
	}

	switch cfg.Backend.Kind {
	case "fake":
	case "homeassistant":
		if cfg.Backend.URL == "" || cfg.Backend.Token == "" {
			return nil, errors.New("the homeassistant backend needs backend.url and a token")
		}
	default:
		return nil, fmt.Errorf("unknown backend kind %q", cfg.Backend.Kind)
	}

	for path, value := range cfg.Gateway.Resources {
		if !json.Valid([]byte(value)) {
			return nil, fmt.Errorf("gateway.resources %q: not valid JSON", path)
		}
	}

	for _, s := range cfg.Scenes {
		if s.Name == "" || s.Name == "All off" || !strings.HasPrefix(s.Entity, "scene.") {
			return nil, fmt.Errorf("scene %q: needs a name other than \"All off\" and a scene.* entity", s.Name)
		}
	}

	seen := map[string]bool{}
	for _, r := range cfg.Rooms {
		if r.Name == "" {
			return nil, errors.New("every room needs a name")
		}
		for _, d := range r.Devices {
			if !strings.Contains(d.Entity, ".") {
				return nil, fmt.Errorf("room %s: %q is not an entity ID", r.Name, d.Entity)
			}
			if seen[d.Entity] {
				return nil, fmt.Errorf("%s is listed twice", d.Entity)
			}
			seen[d.Entity] = true
		}
	}
	return &cfg, nil
}

func secret(env, value, file string) (string, error) {
	if v := os.Getenv(env); v != "" {
		return v, nil
	}
	if file == "" {
		return value, nil
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}
