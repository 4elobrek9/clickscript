//go:build windows

package hotkey

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
	"pointrunner/internal/config"
)

type Action uint8

const (
	Toggle Action = iota
	HoldDown
	HoldUp
	EmergencyStop
	PickScreen
)

func (a Action) String() string {
	switch a {
	case Toggle:
		return "toggle"
	case HoldDown:
		return "hold-down"
	case HoldUp:
		return "hold-up"
	case EmergencyStop:
		return "emergency-stop"
	case PickScreen:
		return "pick-screen"
	default:
		return fmt.Sprintf("unknown(%d)", a)
	}
}

const (
	whKeyboardLL  = 13
	wmKeyDown     = 0x0100
	wmKeyUp       = 0x0101
	wmSysKeyDown  = 0x0104
	wmSysKeyUp    = 0x0105
	wmQuit        = 0x0012
	llkhfInjected = 0x00000010
)

type keyboardEvent struct {
	VKCode    uint32
	ScanCode  uint32
	Flags     uint32
	Time      uint32
	ExtraInfo uintptr
}

type winMessage struct {
	HWnd    windows.Handle
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Point   struct{ X, Y int32 }
	Private uint32
}

var user32 = windows.NewLazySystemDLL("user32.dll")
var kernel32 = windows.NewLazySystemDLL("kernel32.dll")

func Run(ctx context.Context, getConfig func() config.Config, out chan<- Action, loggers ...func(string, ...any)) error {
	var logf func(string, ...any)
	if len(loggers) > 0 {
		logf = loggers[0]
	}
	logEvent := func(format string, args ...any) {
		if logf != nil {
			logf(format, args...)
		}
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	procHook := user32.NewProc("SetWindowsHookExW")
	procUnhook := user32.NewProc("UnhookWindowsHookEx")
	procNext := user32.NewProc("CallNextHookEx")
	procGetMessage := user32.NewProc("GetMessageW")
	procTranslate := user32.NewProc("TranslateMessage")
	procDispatch := user32.NewProc("DispatchMessageW")
	procPostThreadMessage := user32.NewProc("PostThreadMessageW")
	threadID, _, threadErr := kernel32.NewProc("GetCurrentThreadId").Call()
	if threadID == 0 {
		return fmt.Errorf("GetCurrentThreadId failed: %v", threadErr)
	}

	pressed := make(map[uint32]bool)
	hotkeyDown := false
	activeKey := uint32(0)
	activeHotkey := config.Hotkey{}
	holdActive := false
	callback := windows.NewCallback(func(hook, message, data uintptr) uintptr {
		if data != 0 {
			// KBDLLHOOKSTRUCT is valid only for the lifetime of this hook callback.
			event := (*keyboardEvent)(unsafe.Pointer(data))
			if event.Flags&llkhfInjected != 0 {
				result, _, _ := procNext.Call(0, hook, message, data)
				return result
			}
			vk := event.VKCode
			down := message == wmKeyDown || message == wmSysKeyDown
			up := message == wmKeyUp || message == wmSysKeyUp
			wasDown := pressed[vk]
			if down {
				pressed[vk] = true
			}
			cfg := getConfig()
			mods := modifiers(pressed)
			if down && !wasDown && mods.Ctrl && mods.Alt && vk == 0x1B {
				logEvent("global hotkey detected: Ctrl+Alt+Esc emergency stop")
				emit(out, EmergencyStop, logEvent)
			}
			if down && !wasDown && matches(cfg.PickerHotkey, vk, mods) {
				logEvent("global picker hotkey detected: %+v", cfg.PickerHotkey)
				emit(out, PickScreen, logEvent)
			}
			if down && !wasDown && matches(cfg.Hotkey, vk, mods) && !hotkeyDown {
				logEvent("global playback hotkey detected: %+v hold_mode=%t", cfg.Hotkey, cfg.Hold)
				hotkeyDown = true
				activeKey = vk
				activeHotkey = cfg.Hotkey
				if cfg.Hold {
					holdActive = true
					emit(out, HoldDown, logEvent)
				} else {
					emit(out, Toggle, logEvent)
				}
			}
			if up {
				delete(pressed, vk)
				if hotkeyDown && holdActive && (vk == activeKey || !matchesModifiers(activeHotkey, modifiers(pressed))) {
					holdActive = false
					emit(out, HoldUp, logEvent)
				}
				if vk == activeKey {
					hotkeyDown = false
					activeKey = 0
					activeHotkey = config.Hotkey{}
				}
			}
		}
		result, _, _ := procNext.Call(0, hook, message, data)
		return result
	})
	hook, _, callErr := procHook.Call(whKeyboardLL, callback, 0, 0)
	if hook == 0 {
		if err, ok := callErr.(windows.Errno); ok && err != 0 {
			return fmt.Errorf("SetWindowsHookExW failed: errno %d", err)
		}
		return fmt.Errorf("SetWindowsHookExW failed: errno %d", windows.GetLastError())
	}
	logEvent("WH_KEYBOARD_LL installed: handle=%#x", hook)
	defer procUnhook.Call(hook)
	defer logEvent("WH_KEYBOARD_LL uninstalled")
	go func() {
		<-ctx.Done()
		procPostThreadMessage.Call(threadID, wmQuit, 0, 0)
	}()
	var msg winMessage
	for {
		result, _, callErr := procGetMessage.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if int32(result) == -1 {
			return fmt.Errorf("GetMessageW failed: %v", callErr)
		}
		if result == 0 {
			return ctx.Err()
		}
		procTranslate.Call(uintptr(unsafe.Pointer(&msg)))
		procDispatch.Call(uintptr(unsafe.Pointer(&msg)))
	}
}

type modifierState struct{ Ctrl, Alt, Shift, Win bool }

func modifiers(pressed map[uint32]bool) modifierState {
	return modifierState{
		Ctrl:  pressed[0x11] || pressed[0xA2] || pressed[0xA3],
		Alt:   pressed[0x12] || pressed[0xA4] || pressed[0xA5],
		Shift: pressed[0x10] || pressed[0xA0] || pressed[0xA1],
		Win:   pressed[0x5B] || pressed[0x5C],
	}
}

func matches(h config.Hotkey, vk uint32, mods modifierState) bool {
	return vk == keyCode(h.Key) && matchesModifiers(h, mods)
}

func matchesModifiers(h config.Hotkey, mods modifierState) bool {
	return mods.Ctrl == h.Ctrl && mods.Alt == h.Alt &&
		mods.Shift == h.Shift && mods.Win == h.Win
}

func keyCode(key string) uint32 {
	key = strings.ToUpper(key)
	if len(key) == 1 {
		return uint32(key[0])
	}
	if strings.HasPrefix(key, "F") {
		var number int
		if _, err := fmt.Sscanf(key[1:], "%d", &number); err == nil && number >= 1 && number <= 24 {
			if number <= 12 {
				return uint32(0x70 + number - 1)
			}
			return uint32(0x7C + number - 13)
		}
	}
	return 0
}

func emit(out chan<- Action, action Action, loggers ...func(string, ...any)) {
	select {
	case out <- action:
	default:
		if len(loggers) > 0 && loggers[0] != nil {
			loggers[0]("global hotkey action dropped: action=%s reason=action_channel_full", action)
		}
	}
}
