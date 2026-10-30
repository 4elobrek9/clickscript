//go:build windows

package winapi

import (
	"fmt"
	"math"
	"math/rand"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"pointrunner/internal/config"
)

var (
	user32           = windows.NewLazySystemDLL("user32.dll")
	winmm            = windows.NewLazySystemDLL("winmm.dll")
	procMetrics      = user32.NewProc("GetSystemMetrics")
	procSendInput    = user32.NewProc("SendInput")
	procTimeBegin    = winmm.NewProc("timeBeginPeriod")
	procTimeEnd      = winmm.NewProc("timeEndPeriod")
	dpiAwarenessOnce sync.Once
	dpiAwarenessErr  error
)

const (
	smXVirtualScreen  = 76
	smYVirtualScreen  = 77
	smCXVirtualScreen = 78
	smCYVirtualScreen = 79
	inputMouse        = 0

	mouseMove        = 0x0001
	mouseLeftDown    = 0x0002
	mouseLeftUp      = 0x0004
	mouseRightDown   = 0x0008
	mouseRightUp     = 0x0010
	mouseAbsolute    = 0x8000
	mouseVirtualDesk = 0x4000
)

type mouseInput struct {
	Dx, Dy           int32
	MouseData, Flags uint32
	Time             uint32
	ExtraInfo        uintptr
}

type input struct {
	Type       uint32
	MouseInput mouseInput
}

func VirtualScreen() (config.VirtualScreen, error) {
	if err := EnablePerMonitorDPI(); err != nil {
		return config.VirtualScreen{}, err
	}
	if err := procMetrics.Find(); err != nil {
		return config.VirtualScreen{}, fmt.Errorf("resolve GetSystemMetrics: %w", err)
	}
	x, _, _ := procMetrics.Call(smXVirtualScreen)
	y, _, _ := procMetrics.Call(smYVirtualScreen)
	width, _, _ := procMetrics.Call(smCXVirtualScreen)
	height, _, _ := procMetrics.Call(smCYVirtualScreen)
	if width == 0 || height == 0 {
		return config.VirtualScreen{}, fmt.Errorf("GetSystemMetrics returned an empty virtual screen (errno %d)", windows.GetLastError())
	}
	return config.VirtualScreen{X: int(int32(x)), Y: int(int32(y)), Width: int(width), Height: int(height)}, nil
}

func EnablePerMonitorDPI() error {
	dpiAwarenessOnce.Do(func() {
		proc := user32.NewProc("SetProcessDpiAwarenessContext")
		if err := proc.Find(); err != nil {
			dpiAwarenessErr = fmt.Errorf("resolve SetProcessDpiAwarenessContext: %w", err)
			return
		}
		const dpiAwarenessContextPerMonitorAwareV2 = ^uintptr(3)
		result, _, callErr := proc.Call(dpiAwarenessContextPerMonitorAwareV2)
		if result == 0 {
			if errno, ok := callErr.(windows.Errno); ok && errno == windows.ERROR_ACCESS_DENIED {
				return
			}
			dpiAwarenessErr = fmt.Errorf("SetProcessDpiAwarenessContext(PER_MONITOR_AWARE_V2) failed: %v", callErr)
		}
	})
	return dpiAwarenessErr
}

func BeginPreciseTimers() error {
	if err := procTimeBegin.Find(); err != nil {
		return fmt.Errorf("resolve timeBeginPeriod: %w", err)
	}
	result, _, callErr := procTimeBegin.Call(1)
	if result != 0 {
		return fmt.Errorf("timeBeginPeriod(1) failed: errno %d", windows.Errno(result))
	}
	if callErr != nil {
		if errno, ok := callErr.(windows.Errno); !ok || errno != 0 {
			return fmt.Errorf("timeBeginPeriod(1) failed: %w", callErr)
		}
	}
	return nil
}

func EndPreciseTimers() {
	procTimeEnd.Call(1)
}

func Click(point config.Point) error {
	screen, err := VirtualScreen()
	if err != nil {
		return err
	}
	if err := config.ValidatePoints([]config.Point{point}, screen); err != nil {
		return err
	}
	
	// Определяем режим клика (smooth или instant)
	smoothDuration := point.SmoothMoveMS
	clickMode := config.MClickInstant
	if smoothDuration > 0 {
		clickMode = config.MClickSmooth
	}
	
	// Если включен плавный режим, выполняем плавное перемещение
	if smoothDuration > 0 {
		started := time.Now()
		err := SmoothMoveTo(point.X, point.Y, screen.X, screen.Y, smoothDuration)
		elapsed := time.Since(started)
		
		// Логируем плавное перемещение
		if elapsed > 0 && point.SmoothMoveMS > 0 {
			var msg string
			if clickMode == config.MClickSmooth {
				msg = fmt.Sprintf("smooth-move target=(%d,%d) duration=%dms actual=%s deviation=%.1f%%",
					point.X, point.Y, smoothDuration, elapsed.Round(time.Millisecond), point.SmoothDeviationPct)
			} else {
				msg = fmt.Sprintf("smooth-move target=(%d,%d) instant movement duration=%dms actual=%s",
					point.X, point.Y, smoothDuration, elapsed.Round(time.Millisecond))
			}
		}
		
		if err != nil {
			return fmt.Errorf("smooth move: %w", err)
		}
	} else {
		// Старый режим: мгновенное перемещение
		norm := func(value, origin, size int) int32 {
			if size <= 1 {
				return 0
			}
			return int32((int64(value-origin) * 65535) / int64(size-1))
		}
		dx, dy := norm(point.X, screen.X, screen.Width), norm(point.Y, screen.Y, screen.Height)
		moveFlags := uint32(mouseMove | mouseAbsolute | mouseVirtualDesk)
		if err := sendMouse(dx, dy, 0, moveFlags); err != nil {
			return err
		}
	}
	
	// Выполняем клик (то же, что и раньше)
	down, up := uint32(mouseLeftDown), uint32(mouseLeftUp)
	if point.Click == "right" {
		down, up = uint32(mouseRightDown), uint32(mouseRightUp)
	}
	clicks := 1
	if point.Click == "double" {
		clicks = 2
	}
	for i := 0; i < clicks; i++ {
		if err := sendMouse(0, 0, 0, down); err != nil {
			return err
		}
		if err := sendMouse(0, 0, 0, up); err != nil {
			return err
		}
		if clicks == 2 && i == 0 {
			time.Sleep(50 * time.Millisecond)
		}
	}
	
	// Человеческая пауза перед финальным кликом (если включено)
	if point.HumanPauseMS > 0 {
		time.Sleep(time.Duration(point.HumanPauseMS) * time.Millisecond)
	}
	
	return nil
}

func sendMouse(dx, dy int32, data, flags uint32) error {
	event := input{
		Type: inputMouse,
		MouseInput: mouseInput{
			Dx: dx, Dy: dy, MouseData: data, Flags: flags,
		},
	}
	n, _, callErr := procSendInput.Call(1, uintptr(unsafe.Pointer(&event)), unsafe.Sizeof(event))
	if n != 1 {
		if errno, ok := callErr.(windows.Errno); ok && errno != 0 {
			return fmt.Errorf("SendInput failed: errno %d", errno)
		}
		return fmt.Errorf("SendInput inserted %d input events, expected 1 (errno %d)", n, windows.GetLastError())
	}
	return nil
}

// SmoothMoveTo выполняет плавное перемещение к цели с имитацией человеческого поведения
func SmoothMoveTo(targetX, targetY, startX, startY, durationMS int) error {
	if durationMS <= 0 {
		// Мгновенное движение (старое поведение)
		err := sendMouse(int32(targetX-startX), int32(targetY-startY), 0, mouseMove | mouseAbsolute | mouseVirtualDesk)
		return err
	}

	stepDurationMS := durationMS / 50 // 50 шагов за durationMS
	if stepDurationMS == 0 {
		stepDurationMS = 1
	}

	var currentX, currentY float64 = float64(startX), float64(startY)
	
	jitterRand := rand.New(rand.NewSource(time.Now().UnixNano()))
	totalJitterSigma := int32(5 + math.Abs(int64(durationMS/100))) // Чем длиннее движение, тем больше отклонений
	
	for step := 0; step < 50; step++ {
		// Вычисляем текущую позицию
		t := float64(step) / 49.0 // 0..1
		targetXStep := startX + (float64(targetX-startX) * t)
		targetYStep := startY + (float64(targetY-startY) * t)
		
		// Добавляем случайные отклонения для имитации человеческого движения
		dx := int32(jitterRand.NormFloat64() * float64(totalJitterSigma/5))
		dy := int32(jitterRand.NormFloat64() * float64(totalJitterSigma/5))
		
		// Сглаживаем движение (если отклонение слишком большое, возвращаемся ближе к цели)
		if math.Abs(dx)+math.Abs(dy) > stepDurationMS {
			dx = int32(math.Sign(float64(dx)) * min(abs(int64(targetXStep-currentX)), stepDurationMS/2))
			dy = int32(math.Sign(float64(dy)) * min(abs(int64(targetYStep-currentY)), stepDurationMS/2))
		}
		
		err := sendMouse(dx, dy, 0, mouseMove | mouseAbsolute | mouseVirtualDesk)
		if err != nil {
			return fmt.Errorf("smooth move step %d failed: %w", step+1, err)
		}
		
		currentX += float64(dx)
		currentY += float64(dy)
		
		// Задержка для следующего шага
		time.Sleep(time.Duration(stepDurationMS))
	}

	// Финальное точное перемещение в цель (гарантируем, что достигнем цели)
	finalDX := int32(targetX - int(currentX))
	finalDY := int32(targetY - int(currentY))
	
	if finalDX != 0 || finalDY != 0 {
		err := sendMouse(finalDX, finalDY, 0, mouseMove | mouseAbsolute | mouseVirtualDesk)
		if err != nil {
			return fmt.Errorf("smooth move final correction failed: %w", err)
		}
	}

	return nil
}

// min returns the smaller of x or y (inline to avoid import)
func min(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// abs returns the absolute value of x
func abs(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}
