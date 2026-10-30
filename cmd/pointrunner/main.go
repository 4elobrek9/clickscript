//go:build windows

package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"

	"pointrunner/internal/clicker"
	"pointrunner/internal/config"
	"pointrunner/internal/hotkey"
	"pointrunner/internal/logging"
	"pointrunner/internal/ui"
	"pointrunner/internal/winapi"
)

func main() {
	if err := winapi.EnablePerMonitorDPI(); err != nil {
		log.Fatalf("enable per-monitor DPI awareness: %v", err)
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		log.Fatalf("locate application data directory: %v", err)
	}
	dir = filepath.Join(dir, "pointrunner")
	if err = os.MkdirAll(dir, 0700); err != nil {
		log.Fatalf("create application data directory: %v", err)
	}
	logFile, err := logging.Open(filepath.Join(dir, "log.txt"), 5*1024*1024, 3)
	if err != nil {
		log.Fatalf("open log file: %v", err)
	}
	defer logFile.Close()
	logger := log.New(logFile, "pointrunner: ", log.Ldate|log.Ltime|log.Lmicroseconds)
	logger.Printf("application starting: version=development go=%s config_dir=%q log_path=%q", runtime.Version(), dir, filepath.Join(dir, "log.txt"))

	manager, err := config.Load(filepath.Join(dir, "config.json"), winapi.VirtualScreen)
	if err != nil {
		logger.Fatalf("load configuration: %v", err)
	}
	cfg := manager.Get()
	screen, screenErr := winapi.VirtualScreen()
	if screenErr != nil {
		logger.Printf("startup virtual-screen query failed: %v", screenErr)
	} else {
		logger.Printf("startup virtual screen: origin=(%d,%d) dimensions=%dx%d", screen.X, screen.Y, screen.Width, screen.Height)
	}
	logger.Printf("configuration loaded: points=%d playback_hotkey=%+v picker_hotkey=%+v loop=%+v hold=%t",
		len(cfg.Points), cfg.Hotkey, cfg.PickerHotkey, cfg.Loop, cfg.Hold)
	for i, point := range cfg.Points {
		logger.Printf("configured point[%d]: coordinate=(%d,%d) text=%q click=%s delay_before_ms=%d delay_after_ms=%d repeats=%d",
			i+1, point.X, point.Y, point.Text, point.Click, point.PrefixMS, point.SuffixMS, point.Repeats)
	}
	if err = winapi.BeginPreciseTimers(); err != nil {
		logger.Fatalf("enable precise timer: %v", err)
	}
	logger.Printf("Windows timer period set to 1ms")
	defer winapi.EndPreciseTimers()

	runner := clicker.New(func(ctx context.Context, point config.Point) error {
		if point.Text != "" {
			x, y, err := winapi.FindTextWithDiagnostics(ctx, point.Text, logger.Printf, filepath.Join(dir, "debug"))
			if err != nil {
				logger.Printf("find text %q: %v", point.Text, err)
				return err
			}
			point.X, point.Y = x, y
		}
		logger.Printf("sending mouse input: coordinate=(%d,%d) click=%s target_text=%q", point.X, point.Y, point.Click, point.Text)
		err := winapi.Click(point)
		if err != nil {
			logger.Printf("click at (%d, %d): %v", point.X, point.Y, err)
		} else {
			logger.Printf("mouse input succeeded: coordinate=(%d,%d) click=%s", point.X, point.Y, point.Click)
		}
		return err
	}, func(state string) { logger.Printf("runner state: %s", state) }, logger.Printf)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	actions := make(chan hotkey.Action, 8)
	go func() {
		logger.Printf("global keyboard hook starting")
		if err := hotkey.Run(ctx, manager.Get, actions, logger.Printf); err != nil && !errors.Is(err, context.Canceled) {
			logger.Printf("keyboard hook: %v", err)
		}
		logger.Printf("global keyboard hook stopped")
	}()
	app := ui.New(manager, runner, filepath.Join(dir, "profiles"), logger)
	pickScreen := func() {
		logger.Printf("screen point picker started")
		point, err := winapi.PickPoint()
		if err != nil {
			logger.Printf("screen picker: %v", err)
			return
		}
		logger.Printf("screen point picked: coordinate=(%d,%d)", point.X, point.Y)
		cfg := manager.Get()
		cfg.Points = append(cfg.Points, config.Point{X: point.X, Y: point.Y, Click: "left", Repeats: 1})
		if err := manager.Update(cfg); err != nil {
			logger.Printf("save picked point: %v", err)
		} else {
			logger.Printf("picked point saved: total_points=%d", len(cfg.Points))
		}
	}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case action := <-actions:
				logger.Printf("hotkey action received: %s", action)
				switch action {
				case hotkey.EmergencyStop, hotkey.HoldUp:
					runner.Stop()
				case hotkey.PickScreen:
					go pickScreen()
				case hotkey.HoldDown:
					cfg := manager.Get()
					if cfg.Hold {
						if !runner.Start(cfg) {
							logger.Printf("hold hotkey ignored: playback already active")
						}
					}
				case hotkey.Toggle:
					cfg := manager.Get()
					if cfg.Hold {
						logger.Printf("toggle hotkey ignored: hold mode enabled")
						continue
					}
					if runner.State() == clicker.Running {
						runner.Stop()
					} else {
						if !runner.Start(cfg) {
							logger.Printf("toggle hotkey start ignored: playback already active")
						}
					}
				}
			}
		}
	}()
	if err := app.Run(ctx, cancel); err != nil && !errors.Is(err, context.Canceled) {
		logger.Printf("application: %v", err)
	}
	logger.Printf("application shutdown requested")
	cancel()
	runner.Stop()
}
