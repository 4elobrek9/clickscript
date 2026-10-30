package config

import (
	"strings"
	"testing"
)

func TestParseConfig(t *testing.T) {
	cfg, err := Parse([]byte(`{"points":[{"x":4,"y":9,"prefix_ms":0,"suffix_ms":12,"click":"left","repeats":1}],"hotkey":{"ctrl":true,"alt":true,"key":"H"},"loop":{"enabled":true,"iterations":2},"hold":false}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Points) != 1 || cfg.Points[0].Y != 9 || cfg.Loop.Iterations != 2 {
		t.Fatalf("unexpected parsed config: %+v", cfg)
	}
	if cfg.PickerHotkey != (Hotkey{Ctrl: true, Alt: true, Key: "P"}) {
		t.Fatalf("legacy config did not receive default picker hotkey: %+v", cfg.PickerHotkey)
	}
}

func TestParseRejectsInvalidOrTrailingJSON(t *testing.T) {
	for _, input := range []string{
		`{"points":[{"x":0,"y":0,"prefix_ms":-1,"suffix_ms":0,"click":"left","repeats":1}],"hotkey":{"key":"H"}}`,
		`{"points":[],"hotkey":{"key":"H"}} {}`,
		`{"points":[],"hotkey":{"key":"H"},"unexpected":true}`,
	} {
		if _, err := Parse([]byte(input)); err == nil {
			t.Errorf("Parse(%s) unexpectedly succeeded", input)
		}
	}
}

func TestValidateSettingsRejectsDuplicateHotkeys(t *testing.T) {
	cfg := Default()
	cfg.PickerHotkey = cfg.Hotkey
	if err := ValidateSettings(cfg); err == nil || !strings.Contains(err.Error(), "must be different") {
		t.Fatalf("expected duplicate hotkey error, got %v", err)
	}
}

func TestTextTargetsDoNotRequireCoordinatesOnVirtualScreen(t *testing.T) {
	err := ValidatePoints([]Point{{Text: "Continue"}}, VirtualScreen{X: -1920, Y: 0, Width: 1920, Height: 1080})
	if err != nil {
		t.Fatalf("text target should be located by OCR, not saved coordinates: %v", err)
	}
	cfg := Default()
	cfg.Points = []Point{{Text: "Continue", PrefixMS: 150, SuffixMS: 250, Click: "left", Repeats: 1}}
	if err := ValidateSettings(cfg); err != nil {
		t.Fatalf("text target with before/after delays should be valid: %v", err)
	}
}

func TestValidatePointsUsesVirtualScreenOrigin(t *testing.T) {
	screen := VirtualScreen{X: -1920, Y: -200, Width: 3840, Height: 1280}
	if err := ValidatePoints([]Point{{X: -1920, Y: -200}, {X: 1919, Y: 1079}}, screen); err != nil {
		t.Fatal(err)
	}
	for _, point := range []Point{{X: -1921, Y: 0}, {X: 0, Y: 1080}} {
		if err := ValidatePoints([]Point{point}, screen); err == nil || !strings.Contains(err.Error(), "outside") {
			t.Errorf("expected out-of-bounds error for %+v; got %v", point, err)
		}
	}
}
