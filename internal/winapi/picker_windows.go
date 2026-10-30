//go:build windows

package winapi

import (
	"fmt"
	"runtime"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
	"pointrunner/internal/config"
)

const (
	wmDestroy      = 0x0002
	wmClose        = 0x0010
	wmKeyDown      = 0x0100
	wmLButtonDown  = 0x0201
	wsPopup        = 0x80000000
	wsVisible      = 0x10000000
	wsExTopmost    = 0x00000008
	wsExToolwindow = 0x00000080
	wsExLayered    = 0x00080000
	lwaAlpha       = 0x00000002
	idcCross       = 32515
)

type point struct {
	X int32
	Y int32
}

type winMessage struct {
	HWnd    windows.Handle
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Point   point
	Private uint32
}

type windowClassEx struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   windows.Handle
	Icon       windows.Handle
	Cursor     windows.Handle
	Background windows.Handle
	MenuName   *uint16
	ClassName  *uint16
	IconSmall  windows.Handle
}

type pickResult struct {
	point config.Point
	err   error
}

var pickerResults sync.Map
var pickerMu sync.Mutex

func PickPoint() (config.Point, error) {
	pickerMu.Lock()
	defer pickerMu.Unlock()
	result := make(chan pickResult, 1)
	go runPicker(result)
	picked := <-result
	return picked.point, picked.err
}

func runPicker(result chan<- pickResult) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	screen, err := VirtualScreen()
	if err != nil {
		result <- pickResult{err: err}
		return
	}
	dll := user32
	registerClass := dll.NewProc("RegisterClassExW")
	createWindow := dll.NewProc("CreateWindowExW")
	destroyWindow := dll.NewProc("DestroyWindow")
	defWindowProc := dll.NewProc("DefWindowProcW")
	getMessage := dll.NewProc("GetMessageW")
	translateMessage := dll.NewProc("TranslateMessage")
	dispatchMessage := dll.NewProc("DispatchMessageW")
	setLayered := dll.NewProc("SetLayeredWindowAttributes")
	loadCursor := dll.NewProc("LoadCursorW")
	postQuit := dll.NewProc("PostQuitMessage")
	getCursorPos := dll.NewProc("GetCursorPos")
	className, _ := windows.UTF16PtrFromString("PointRunnerPickerWindow")
	callback := windows.NewCallback(func(hwnd, message, wparam, lparam uintptr) uintptr {
		switch message {
		case wmLButtonDown:
			var cursor point
			ok, _, callErr := getCursorPos.Call(uintptr(unsafe.Pointer(&cursor)))
			if ok == 0 {
				pickerResults.Store(hwnd, pickResult{err: fmt.Errorf("GetCursorPos failed: %v", callErr)})
			} else {
				pickerResults.Store(hwnd, pickResult{point: config.Point{X: int(cursor.X), Y: int(cursor.Y)}})
			}
			destroyWindow.Call(hwnd)
			return 0
		case wmKeyDown:
			if wparam == 0x1B {
				pickerResults.Store(hwnd, pickResult{err: fmt.Errorf("screen picking cancelled")})
				destroyWindow.Call(hwnd)
				return 0
			}
		case wmDestroy:
			postQuit.Call(0)
			return 0
		}
		ret, _, _ := defWindowProc.Call(hwnd, message, wparam, lparam)
		return ret
	})
	cursor, _, cursorErr := loadCursor.Call(0, idcCross)
	if cursor == 0 {
		result <- pickResult{err: fmt.Errorf("LoadCursorW failed: %v", cursorErr)}
		return
	}
	class := windowClassEx{
		Size: uint32(unsafe.Sizeof(windowClassEx{})), WndProc: callback,
		Instance: windows.Handle(0), Cursor: windows.Handle(cursor), ClassName: className,
	}
	atom, _, classErr := registerClass.Call(uintptr(unsafe.Pointer(&class)))
	if atom == 0 {
		if errno, ok := classErr.(windows.Errno); !ok || errno != windows.ERROR_CLASS_ALREADY_EXISTS {
			result <- pickResult{err: fmt.Errorf("RegisterClassExW failed: %v", classErr)}
			return
		}
	}
	title, _ := windows.UTF16PtrFromString("Click the exact screen point to add it. Esc cancels.")
	hwnd, _, createErr := createWindow.Call(
		wsExTopmost|wsExToolwindow|wsExLayered, uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(title)), wsPopup|wsVisible, uintptr(int32(screen.X)), uintptr(int32(screen.Y)),
		uintptr(screen.Width), uintptr(screen.Height), 0, 0, 0, 0,
	)
	if hwnd == 0 {
		result <- pickResult{err: fmt.Errorf("CreateWindowExW failed: %v", createErr)}
		return
	}
	pickerResults.Store(hwnd, pickResult{})
	layered, _, layeredErr := setLayered.Call(hwnd, 0, 70, lwaAlpha)
	if layered == 0 {
		destroyWindow.Call(hwnd)
		pickerResults.Delete(hwnd)
		result <- pickResult{err: fmt.Errorf("SetLayeredWindowAttributes failed: %v", layeredErr)}
		return
	}
	var msg winMessage
	for {
		status, _, getErr := getMessage.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if int32(status) == -1 {
			pickerResults.Delete(hwnd)
			result <- pickResult{err: fmt.Errorf("GetMessageW for picker failed: %v", getErr)}
			return
		}
		if status == 0 {
			break
		}
		translateMessage.Call(uintptr(unsafe.Pointer(&msg)))
		dispatchMessage.Call(uintptr(unsafe.Pointer(&msg)))
	}
	value, ok := pickerResults.LoadAndDelete(hwnd)
	if !ok {
		result <- pickResult{err: fmt.Errorf("picker closed without a selection")}
		return
	}
	result <- value.(pickResult)
}
