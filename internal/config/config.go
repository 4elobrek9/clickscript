package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

type Point struct {
	X        int    `json:"x"`
	Y        int    `json:"y"`
	Text     string `json:"text,omitempty"`
	PrefixMS int    `json:"prefix_ms"`
	SuffixMS int    `json:"suffix_ms"`
	Click    string `json:"click"`
	Repeats  int    `json:"repeats"`
	// Smooth movement: true to enable gradual cursor animation to target
	// Duration in milliseconds; 0 means instant move (default old behavior)
	SmoothMoveMS      int   `json:"smooth_move_ms,omitempty"`
	SmoothDeviationPct float64 `json:"smooth_deviation_pct,omitempty"` // Random deviation percentage (0-50) for human-like jitter
	// Human behavior: pause range in milliseconds before final click (0=disabled)
	HumanPauseMS      int    `json:"human_pause_ms,omitempty"`
}

type SmoothMode struct {
	Enabled          bool     `json:"enabled"`           // Enable smooth movement mode globally
	DurationMS       int      `json:"duration_ms"`       // Fixed duration for smooth move (500-3000ms, 0=instant)
	DeviationPct     float64  `json:"deviation_pct"`     // Random deviation percentage (0-50)
	JitterEnabled    bool     `json:"jitter_enabled"`    // Enable mouse jitter during move
	JitterSigmaMS    int      `json:"jitter_sigma_ms"`   // Jitter magnitude in milliseconds
	SearchTargets    []string `json:"search_targets"`    // Optional moving targets to find and follow
}

type ClickMode string

const (
	MClickInstant  ClickMode = "instant" // Default: instant move + click (old behavior)
	MClickSmooth   ClickMode = "smooth"  // New: gradual move with human-like jitter + final click
)

type Hotkey struct {
	Ctrl  bool   `json:"ctrl"`
	Alt   bool   `json:"alt"`
	Shift bool   `json:"shift"`
	Win   bool   `json:"win"`
	Key   string `json:"key"`
}

type Loop struct {
	Enabled    bool `json:"enabled"`
	Iterations int  `json:"iterations"`
	Infinite   bool `json:"infinite"`
}

const maxDelayMS int64 = 9_223_372_036_854

type Config struct {
	Points       []Point `json:"points"`
	Hotkey       Hotkey  `json:"hotkey"`
	PickerHotkey Hotkey  `json:"picker_hotkey"`
	Loop         Loop    `json:"loop"`
	SmoothMode   SmoothMode `json:"smooth_mode,omitempty"` // Enable smooth movement globally (optional)
	Hold         bool     `json:"hold"`
}

type VirtualScreen struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

type ScreenProvider func() (VirtualScreen, error)

type Manager struct {
	mu       sync.RWMutex
	config   Config
	path     string
	screenFn ScreenProvider
}

func Default() Config {
	return Config{
		Points:       []Point{},
		Hotkey:       Hotkey{Ctrl: true, Alt: true, Key: "H"},
		PickerHotkey: Hotkey{Ctrl: true, Alt: true, Key: "P"},
		Loop:         Loop{Enabled: true, Iterations: 1},
		SmoothMode:   SmoothMode{Enabled: false, DurationMS: 0, DeviationPct: 0, JitterEnabled: false, JitterSigmaMS: 5}, // По умолчанию мгновенный клик (старое поведение)
	Hold:         false,
	}
}

func Parse(data []byte) (Config, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var cfg Config
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		if err == nil {
			return Config{}, errors.New("decode config: trailing JSON value")
		}
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	if cfg.Points == nil {
		cfg.Points = []Point{}
	}
	if cfg.PickerHotkey.Key == "" {
		cfg.PickerHotkey = Default().PickerHotkey
	}
	if err := ValidateSettings(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func ValidateSettings(cfg Config) error {
	if err := validateHotkey(cfg.Hotkey); err != nil {
		return fmt.Errorf("playback hotkey: %w", err)
	}
	if err := validateHotkey(cfg.PickerHotkey); err != nil {
		return fmt.Errorf("picker hotkey: %w", err)
	}
	if cfg.Hotkey == cfg.PickerHotkey {
		return errors.New("playback and picker hotkeys must be different")
	}
	copyHotkey := Hotkey{Ctrl: true, Alt: true, Key: "T"}
	if cfg.Hotkey == copyHotkey || cfg.PickerHotkey == copyHotkey {
		return errors.New("Ctrl+Alt+T is reserved for copying selected text")
	}
	if cfg.Loop.Enabled && !cfg.Loop.Infinite && cfg.Loop.Iterations < 1 {
		return errors.New("loop iterations must be at least 1")
	}
	for i, point := range cfg.Points {
		if point.PrefixMS < 0 || point.SuffixMS < 0 {
			return fmt.Errorf("point %d: delays must be >= 0", i+1)
		}
		if int64(point.PrefixMS) > maxDelayMS || int64(point.SuffixMS) > maxDelayMS {
			return fmt.Errorf("point %d: delay exceeds the supported duration", i+1)
		}
		if point.Repeats < 1 {
			return fmt.Errorf("point %d: repeats must be at least 1", i+1)
		}
		if point.Click != "left" && point.Click != "right" && point.Click != "double" {
			return fmt.Errorf("point %d: click must be left, right, or double", i+1)
		}
	}
	return nil
}

func validateHotkey(hotkey Hotkey) error {
	if strings.TrimSpace(hotkey.Key) == "" {
		return errors.New("key is required")
	}
	if hotkey.Key != strings.ToUpper(hotkey.Key) {
		return errors.New("key must be uppercase")
	}
	key := hotkey.Key
	valid := len(key) == 1 && ((key[0] >= 'A' && key[0] <= 'Z') || (key[0] >= '0' && key[0] <= '9'))
	if strings.HasPrefix(key, "F") && len(key) <= 3 {
		var number int
		_, err := fmt.Sscanf(key[1:], "%d", &number)
		valid = err == nil && number >= 1 && number <= 24 && fmt.Sprintf("F%d", number) == key
	}
	if !valid {
		return fmt.Errorf("unsupported hotkey key %q", key)
	}
	return nil
}

func ValidatePoints(points []Point, screen VirtualScreen) error {
	if screen.Width <= 0 || screen.Height <= 0 {
		return errors.New("virtual screen dimensions must be positive")
	}
	right, bottom := screen.X+screen.Width, screen.Y+screen.Height
	for i, point := range points {
		if strings.TrimSpace(point.Text) != "" {
			continue
		}
		if point.X < screen.X || point.X >= right || point.Y < screen.Y || point.Y >= bottom {
			return fmt.Errorf("point %d: coordinates (%d, %d) are outside the virtual screen", i+1, point.X, point.Y)
		}
	}
	return nil
}

func Load(path string, screenFn ScreenProvider) (*Manager, error) {
	cfg := Default()
	data, err := os.ReadFile(path)
	missing := errors.Is(err, os.ErrNotExist)
	if err == nil {
		cfg, err = Parse(data)
		if err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read config: %w", err)
	}
	m := &Manager{config: cfg, path: path, screenFn: screenFn}
	if err := m.validate(cfg); err != nil {
		return nil, err
	}
	if missing {
		if err = m.Save(cfg); err != nil {
			return nil, err
		}
	}
	return m, nil
}

func (m *Manager) validate(cfg Config) error {
	if err := ValidateSettings(cfg); err != nil {
		return err
	}
	if m.screenFn != nil {
		screen, err := m.screenFn()
		if err != nil {
			return fmt.Errorf("get virtual screen: %w", err)
		}
		if err := ValidatePoints(cfg.Points, screen); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) Get() Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return clone(m.config)
}

func (m *Manager) Hotkey() Hotkey {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.config.Hotkey
}

func (m *Manager) Update(cfg Config) error {
	if err := m.validate(cfg); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.write(cfg); err != nil {
		return err
	}
	m.config = clone(cfg)
	return nil
}

func (m *Manager) Save(cfg Config) error {
	if err := m.validate(cfg); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.write(cfg)
}

func (m *Manager) write(cfg Config) error {
	if err := os.MkdirAll(filepath.Dir(m.path), 0700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	if err := os.WriteFile(m.path, append(data, '\n'), 0600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}

func clone(cfg Config) Config {
	cfg.Points = append([]Point(nil), cfg.Points...)
	return cfg
}
