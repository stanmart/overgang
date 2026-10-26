package tradfri

import "testing"

func TestWhitePresets(t *testing.T) {
	for _, p := range WhitePresets {
		if m, ok := PresetMireds(p.Hex); !ok || m != p.Mireds {
			t.Errorf("%s: %d, %v", p.Hex, m, ok)
		}
		if got := NearestPreset(p.Mireds); got != p.Hex {
			t.Errorf("NearestPreset(%d) = %s, want %s", p.Mireds, got, p.Hex)
		}
	}
	// What a real gateway on firmware 1.21.57 reports for a bulb at 3000 K.
	if got := NearestPreset(333); got != "f2eccf" {
		t.Errorf("NearestPreset(333) = %s", got)
	}
}
