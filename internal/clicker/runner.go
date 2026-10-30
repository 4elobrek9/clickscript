package clicker

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"pointrunner/internal/config"
)

type State string

const (
	Idle    State = "idle"
	Running State = "running"
	Paused  State = "paused"
)

type ClickFunc func(context.Context, config.Point) error
type StateFunc func(string)
type LogFunc func(string, ...any)

type Runner struct {
	mu      sync.Mutex
	state   State
	lastErr string
	cancel  context.CancelFunc
	changed chan struct{}
	click   ClickFunc
	onState StateFunc
	logf    LogFunc
	runID   uint64
}

func New(click ClickFunc, onState StateFunc, loggers ...LogFunc) *Runner {
	r := &Runner{state: Idle, changed: make(chan struct{}, 1), click: click, onState: onState}
	if len(loggers) > 0 {
		r.logf = loggers[0]
	}
	return r
}

func (r *Runner) State() State {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state
}

func (r *Runner) LastError() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastErr
}

func (r *Runner) Start(cfg config.Config) bool {
	r.mu.Lock()
	if r.state != Idle {
		r.mu.Unlock()
		return false
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	r.state = Running
	r.lastErr = ""
	r.runID++
	runID := r.runID
	r.signalLocked()
	r.mu.Unlock()
	r.log("playback run=%d started: points=%d loop_enabled=%t iterations=%d infinite=%t hold=%t",
		runID, len(cfg.Points), cfg.Loop.Enabled, cfg.Loop.Iterations, cfg.Loop.Infinite, cfg.Hold)
	r.notify(Running)
	go r.run(ctx, runID, cfg)
	return true
}

func (r *Runner) Stop() {
	r.mu.Lock()
	cancel := r.cancel
	active := cancel != nil
	runID := r.runID
	r.mu.Unlock()
	if cancel != nil {
		r.log("playback run=%d stop requested", runID)
		cancel()
	} else if !active {
		r.log("playback stop requested while idle")
	}
}

func (r *Runner) Pause() {
	r.setState(Paused)
}

func (r *Runner) Resume() {
	r.setState(Running)
}

func (r *Runner) setState(state State) {
	r.mu.Lock()
	if r.cancel == nil || r.state == Idle {
		r.mu.Unlock()
		return
	}
	r.state = state
	runID := r.runID
	r.signalLocked()
	r.mu.Unlock()
	r.log("playback run=%d state changed to %s", runID, state)
	r.notify(state)
}

func (r *Runner) signalLocked() {
	select {
	case r.changed <- struct{}{}:
	default:
	}
}

func (r *Runner) notify(state State) {
	if r.onState != nil {
		r.onState(string(state))
	}
}

func (r *Runner) run(ctx context.Context, runID uint64, cfg config.Config) {
	defer func() {
		if value := recover(); value != nil {
			r.log("playback run=%d recovered panic: %v", runID, value)
			r.setError(fmt.Sprintf("runner panic: %v", value))
		}
		r.mu.Lock()
		r.cancel = nil
		r.state = Idle
		r.signalLocked()
		r.mu.Unlock()
		r.log("playback run=%d finished; state=idle error=%q", runID, r.LastError())
		r.notify(Idle)
	}()
	if len(cfg.Points) == 0 {
		r.log("playback run=%d ended without actions: no points configured", runID)
		return
	}
	iterations := 1
	if cfg.Loop.Enabled {
		iterations = cfg.Loop.Iterations
	}
	r.log("playback run=%d execution plan: passes=%d infinite=%t", runID, iterations, cfg.Loop.Enabled && cfg.Loop.Infinite)
	for round := 0; cfg.Loop.Enabled && cfg.Loop.Infinite || round < iterations; round++ {
		r.log("playback run=%d pass=%d started", runID, round+1)
		for pointIndex, point := range cfg.Points {
			for i := 0; i < point.Repeats; i++ {
				r.log("playback run=%d pass=%d step=%d repeat=%d/%d target=%q coordinate=(%d,%d) click=%s delay_before_ms=%d delay_after_ms=%d smooth_mode=%t duration_ms=%d deviation_pct=%.1f%% human_pause_ms=%d",
					runID, round+1, pointIndex+1, i+1, point.Repeats, point.Text, point.X, point.Y, point.Click, point.PrefixMS, point.SuffixMS, 
					point.SmoothMoveMS > 0, point.SmoothMoveMS, point.SmoothDeviationPct, point.HumanPauseMS)
				if err := r.sleep(ctx, time.Duration(point.PrefixMS)*time.Millisecond); err != nil {
					r.log("playback run=%d cancelled/error during pre-click wait: %v", runID, err)
					return
				}
				if err := r.waitUnpaused(ctx); err != nil {
					r.log("playback run=%d cancelled while paused: %v", runID, err)
					return
				}
				started := time.Now()
				if err := safeClick(ctx, r.click, point); err != nil {
					r.log("playback run=%d pass=%d step=%d click failed after=%s: %v", runID, round+1, pointIndex+1, time.Since(started), err)
					r.setError(err.Error())
					return
				}
				r.log("playback run=%d pass=%d step=%d click completed in %s", runID, round+1, pointIndex+1, time.Since(started))
				if err := r.sleep(ctx, time.Duration(point.SuffixMS)*time.Millisecond); err != nil {
					r.log("playback run=%d cancelled/error during post-click wait: %v", runID, err)
					return
				}
			}
		}
		r.log("playback run=%d pass=%d completed", runID, round+1)
		if !cfg.Loop.Enabled {
			break
		}
	}
}

func (r *Runner) setError(message string) {
	r.mu.Lock()
	r.lastErr = message
	r.mu.Unlock()
	r.notify(State("error: " + message))
}

func (r *Runner) log(format string, args ...any) {
	if r.logf != nil {
		r.logf(format, args...)
	}
}

func safeClick(ctx context.Context, click ClickFunc, point config.Point) (err error) {
	defer func() {
		if value := recover(); value != nil {
			err = fmt.Errorf("click callback panicked: %v", value)
		}
	}()
	if click == nil {
		return fmt.Errorf("click callback is not configured")
	}
	return click(ctx, point)
}

func (r *Runner) sleep(ctx context.Context, duration time.Duration) error {
	remaining := duration
	for remaining > 0 {
		if err := r.waitUnpaused(ctx); err != nil {
			return err
		}
		start := time.Now()
		timer := time.NewTimer(remaining)
		select {
		case <-timer.C:
			return nil
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return ctx.Err()
		case <-r.changed:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			elapsed := time.Since(start)
			if elapsed >= remaining {
				return nil
			}
			remaining -= elapsed
		}
	}
	return nil
}

func (r *Runner) waitUnpaused(ctx context.Context) error {
	for {
		r.mu.Lock()
		paused := r.state == Paused
		r.mu.Unlock()
		if !paused {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-r.changed:
		}
	}
}

var ErrStopped = errors.New("runner stopped")
