//go:build windows

package ui

import (
	"context"
	"embed"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/getlantern/systray"
	"pointrunner/internal/clicker"
	"pointrunner/internal/config"
	"pointrunner/internal/winapi"
)

//go:embed index.html
var assets embed.FS

var profilePattern = regexp.MustCompile(`^[A-Za-z0-9 _-]{1,40}$`)

type App struct {
	manager    *config.Manager
	runner     *clicker.Runner
	profileDir string
	logger     *log.Logger
}

func New(manager *config.Manager, runner *clicker.Runner, profileDir string, logger *log.Logger) *App {
	return &App{manager: manager, runner: runner, profileDir: profileDir, logger: logger}
}

func (a *App) Run(ctx context.Context, cancel context.CancelFunc) error {
	listener, err := net.Listen("tcp", "127.0.0.1:8765")
	if err != nil {
		return fmt.Errorf("listen on 127.0.0.1:8765: %w", err)
	}
	a.logger.Printf("web server listening: address=%s", listener.Addr())
	server := &http.Server{Handler: a.routes(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			a.logger.Printf("web server: %v", err)
			cancel()
		} else {
			a.logger.Printf("web server stopped")
		}
	}()
	go func() {
		<-ctx.Done()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer shutdownCancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			a.logger.Printf("web server shutdown failed: %v", err)
		}
		systray.Quit()
	}()

	a.logger.Printf("system tray starting")
	systray.Run(func() {
		systray.SetIcon(trayIcon())
		systray.SetTitle("PointRunner")
		systray.SetTooltip("PointRunner - idle")
		start := systray.AddMenuItem("Start / stop", "Toggle point playback")
		pause := systray.AddMenuItem("Pause", "Pause playback")
		open := systray.AddMenuItem("Open settings", "Open the local settings page")
		systray.AddSeparator()
		exit := systray.AddMenuItem("Exit", "Exit PointRunner")
		a.logger.Printf("system tray ready: menu=start-stop,pause,open-settings,exit")
		go a.trayActions(ctx, cancel, start, pause, open, exit)
	}, func() {
		a.logger.Printf("system tray exiting")
		cancel()
	})
	return nil
}

func (a *App) trayActions(ctx context.Context, cancel context.CancelFunc, start, pause, open, exit *systray.MenuItem) {
	ticker := time.NewTicker(400 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-start.ClickedCh:
			a.logger.Printf("tray action: start/stop")
			if a.runner.State() == clicker.Running {
				a.runner.Stop()
			} else {
				a.runner.Start(a.manager.Get())
			}
		case <-pause.ClickedCh:
			a.logger.Printf("tray action: pause/resume")
			if a.runner.State() == clicker.Paused {
				a.runner.Resume()
			} else {
				a.runner.Pause()
			}
		case <-open.ClickedCh:
			a.logger.Printf("tray action: open settings")
			if err := exec.Command("rundll32.exe", "url.dll,FileProtocolHandler", "http://127.0.0.1:8765").Start(); err != nil {
				a.logger.Printf("open settings: %v", err)
			}
		case <-exit.ClickedCh:
			a.logger.Printf("tray action: exit")
			cancel()
			systray.Quit()
		case <-ticker.C:
			state := a.runner.State()
			systray.SetTooltip("PointRunner - " + string(state))
			if state == clicker.Paused {
				pause.SetTitle("Resume")
			} else {
				pause.SetTitle("Pause")
			}
		}
	}
}

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		http.ServeFileFS(w, r, assets, "index.html")
	})
	mux.HandleFunc("/api/config", a.handleConfig)
	mux.HandleFunc("/api/screen", a.handleScreen)
	mux.HandleFunc("/api/status", a.handleStatus)
	mux.HandleFunc("/api/pick", a.handlePick)
	mux.HandleFunc("/api/action/", a.handleAction)
	mux.HandleFunc("/api/profiles", a.handleProfiles)
	mux.HandleFunc("/api/profiles/", a.handleProfile)
	return a.logRequests(hostOnly(mux))
}

type responseRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *responseRecorder) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *responseRecorder) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(data)
	w.bytes += n
	return n, err
}

func (w *responseRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (a *App) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/status" {
			next.ServeHTTP(w, r)
			return
		}
		started := time.Now()
		recorder := &responseRecorder{ResponseWriter: w}
		next.ServeHTTP(recorder, r)
		status := recorder.status
		if status == 0 {
			status = http.StatusOK
		}
		if status >= 400 {
			a.logger.Printf("HTTP request failed: method=%s path=%q status=%d elapsed=%s bytes=%d",
				r.Method, r.URL.Path, status, time.Since(started), recorder.bytes)
		} else {
			a.logger.Printf("HTTP request complete: method=%s path=%q status=%d elapsed=%s bytes=%d",
				r.Method, r.URL.Path, status, time.Since(started), recorder.bytes)
		}
	})
}

func hostOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "127.0.0.1:8765" && r.Host != "localhost:8765" {
			// The outer request logger records the denied response without logging untrusted headers.
			http.Error(w, "invalid Host header", http.StatusForbidden)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && origin != "http://127.0.0.1:8765" && origin != "http://localhost:8765" {
			http.Error(w, "invalid Origin header", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *App) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, a.manager.Get())
	case http.MethodPut:
		data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			a.logger.Printf("configuration update body read failed: %v", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		cfg, err := config.Parse(data)
		if err == nil {
			err = a.manager.Update(cfg)
		}
		if err != nil {
			a.logger.Printf("configuration update rejected: %v", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		a.logger.Printf("configuration updated: points=%d playback_hotkey=%+v picker_hotkey=%+v loop=%+v hold=%t",
			len(cfg.Points), cfg.Hotkey, cfg.PickerHotkey, cfg.Loop, cfg.Hold)
		for i, point := range cfg.Points {
			a.logger.Printf("updated point[%d]: coordinate=(%d,%d) text=%q click=%s delay_before_ms=%d delay_after_ms=%d repeats=%d",
				i+1, point.X, point.Y, point.Text, point.Click, point.PrefixMS, point.SuffixMS, point.Repeats)
		}
		writeJSON(w, http.StatusOK, a.manager.Get())
	default:
		w.Header().Set("Allow", "GET, PUT")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *App) handleScreen(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	screen, err := winapi.VirtualScreen()
	if err != nil {
		a.logger.Printf("virtual screen request failed: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, screen)
}

func (a *App) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"state":  string(a.runner.State()),
		"points": len(a.manager.Get().Points),
		"error":  a.runner.LastError(),
	})
}

func (a *App) handlePick(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	point, err := winapi.PickPoint()
	if err != nil {
		a.logger.Printf("screen point picker failed: %v", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	a.logger.Printf("screen point picked: coordinate=(%d,%d)", point.X, point.Y)
	cfg := a.manager.Get()
	cfg.Points = append(cfg.Points, config.Point{X: point.X, Y: point.Y, Click: "left", Repeats: 1})
	if err := a.manager.Update(cfg); err != nil {
		a.logger.Printf("picked point could not be saved: coordinate=(%d,%d) error=%v", point.X, point.Y, err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	a.logger.Printf("picked point saved: total_points=%d", len(cfg.Points))
	writeJSON(w, http.StatusOK, a.manager.Get())
}

func (a *App) handleAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	action := strings.TrimPrefix(r.URL.Path, "/api/action/")
	a.logger.Printf("playback API action requested: action=%s", action)
	switch action {
	case "start":
		cfg := a.manager.Get()
		if len(cfg.Points) == 0 {
			a.logger.Printf("playback API start rejected: no points configured")
			http.Error(w, "add at least one point before starting", http.StatusBadRequest)
			return
		}
		if !a.runner.Start(cfg) {
			a.logger.Printf("playback API start rejected: runner already active")
			http.Error(w, "playback is already active", http.StatusConflict)
			return
		}
	case "stop":
		a.runner.Stop()
	case "pause":
		a.runner.Pause()
	case "resume":
		a.runner.Resume()
	default:
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"state": string(a.runner.State())})
}

func (a *App) handleProfiles(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		entries, err := os.ReadDir(a.profileDir)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			a.logger.Printf("profile list failed: directory=%q error=%v", a.profileDir, err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			if !entry.IsDir() && filepath.Ext(entry.Name()) == ".json" {
				names = append(names, strings.TrimSuffix(entry.Name(), ".json"))
			}
		}
		a.logger.Printf("profile list requested: count=%d", len(names))
		writeJSON(w, http.StatusOK, names)
	case http.MethodPost:
		var request struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&request); err != nil {
			a.logger.Printf("profile save request decode failed: %v", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if !profilePattern.MatchString(request.Name) {
			a.logger.Printf("profile save rejected: invalid name=%q", request.Name)
			http.Error(w, "profile name must be 1-40 letters, digits, spaces, hyphens, or underscores", http.StatusBadRequest)
			return
		}
		if err := os.MkdirAll(a.profileDir, 0700); err != nil {
			a.logger.Printf("profile directory creation failed: directory=%q error=%v", a.profileDir, err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		data, err := json.MarshalIndent(a.manager.Get(), "", "  ")
		if err == nil {
			err = os.WriteFile(filepath.Join(a.profileDir, request.Name+".json"), append(data, '\n'), 0600)
		}
		if err != nil {
			a.logger.Printf("profile save failed: name=%q error=%v", request.Name, err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		a.logger.Printf("profile saved: name=%q", request.Name)
		writeJSON(w, http.StatusCreated, map[string]string{"name": request.Name})
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *App) handleProfile(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/api/profiles/")
	if !profilePattern.MatchString(name) {
		http.Error(w, "invalid profile name", http.StatusBadRequest)
		return
	}
	if r.Method == http.MethodDelete {
		err := os.Remove(filepath.Join(a.profileDir, name+".json"))
		if err != nil {
			a.logger.Printf("profile delete failed: name=%q error=%v", name, err)
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		a.logger.Printf("profile deleted: name=%q", name)
		writeJSON(w, http.StatusOK, map[string]string{"deleted": name})
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	data, err := os.ReadFile(filepath.Join(a.profileDir, name+".json"))
	if err != nil {
		a.logger.Printf("profile load failed: name=%q error=%v", name, err)
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	cfg, err := config.Parse(data)
	if err == nil {
		err = a.manager.Update(cfg)
	}
	if err != nil {
		a.logger.Printf("profile load rejected: name=%q error=%v", name, err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	a.logger.Printf("profile loaded: name=%q points=%d", name, len(cfg.Points))
	writeJSON(w, http.StatusOK, a.manager.Get())
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		return
	}
}

func trayIcon() []byte {
	const size = 16
	pixelBytes, maskBytes := size*size*4, ((size+31)/32)*4*size
	imageOffset := 6 + 16 + 40
	icon := make([]byte, imageOffset+pixelBytes+maskBytes)
	binary.LittleEndian.PutUint16(icon[2:4], 1)
	binary.LittleEndian.PutUint16(icon[4:6], 1)
	icon[6], icon[7] = size, size
	binary.LittleEndian.PutUint16(icon[10:12], 1)
	binary.LittleEndian.PutUint16(icon[12:14], 32)
	binary.LittleEndian.PutUint32(icon[14:18], uint32(pixelBytes+maskBytes))
	binary.LittleEndian.PutUint32(icon[18:22], uint32(imageOffset))
	binary.LittleEndian.PutUint32(icon[22:26], 40)
	binary.LittleEndian.PutUint32(icon[26:30], size)
	binary.LittleEndian.PutUint32(icon[30:34], size*2)
	binary.LittleEndian.PutUint16(icon[34:36], 1)
	binary.LittleEndian.PutUint16(icon[36:38], 32)
	binary.LittleEndian.PutUint32(icon[42:46], uint32(pixelBytes))
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			offset := imageOffset + ((size-1-y)*size+x)*4
			if x == size/2 || y == size/2 || x == y || x+y == size-1 {
				icon[offset], icon[offset+1], icon[offset+2], icon[offset+3] = 45, 170, 245, 255
			} else {
				icon[offset+3] = 255
			}
		}
	}
	return icon
}
