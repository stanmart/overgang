package tradfri

import (
	"fmt"
	"math"
	"strconv"
)

// PresetMireds returns the colour temperature a white preset hex stands for.
func PresetMireds(hex string) (int, bool) {
	for _, p := range WhitePresets {
		if p.Hex == hex {
			return p.Mireds, true
		}
	}
	return 0, false
}

// NearestPreset returns the white preset hex closest to a colour temperature, which is
// what the app highlights for a white-spectrum bulb.
func NearestPreset(mireds int) string {
	best := WhitePresets[0]
	for _, p := range WhitePresets[1:] {
		if abs(p.Mireds-mireds) < abs(best.Mireds-mireds) {
			best = p
		}
	}
	return best.Hex
}

// HexToXY converts an sRGB hex colour to CIE 1931 xy.
func HexToXY(hex string) (x, y float64, err error) {
	if len(hex) != 6 {
		return 0, 0, fmt.Errorf("colour %q: want 6 hex digits", hex)
	}
	v, err := strconv.ParseUint(hex, 16, 32)
	if err != nil {
		return 0, 0, fmt.Errorf("colour %q: %w", hex, err)
	}
	r := linear(float64(v>>16&0xff) / 255)
	g := linear(float64(v>>8&0xff) / 255)
	b := linear(float64(v&0xff) / 255)
	X := 0.4124*r + 0.3576*g + 0.1805*b
	Y := 0.2126*r + 0.7152*g + 0.0722*b
	Z := 0.0193*r + 0.1192*g + 0.9505*b
	sum := X + Y + Z
	if sum == 0 {
		return 0.3127, 0.3290, nil // black has no chromaticity; use the white point
	}
	return X / sum, Y / sum, nil
}

// RGBToHex formats 0–255 channels as the app's hex colour.
func RGBToHex(r, g, b float64) string {
	return fmt.Sprintf("%02x%02x%02x", clampByte(r), clampByte(g), clampByte(b))
}

func linear(c float64) float64 {
	if c <= 0.04045 {
		return c / 12.92
	}
	return math.Pow((c+0.055)/1.055, 2.4)
}

func clampByte(v float64) int {
	return int(math.Round(math.Max(0, math.Min(255, v))))
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
