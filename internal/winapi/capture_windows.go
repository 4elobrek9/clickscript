//go:build windows

package winapi

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
	"pointrunner/internal/config"
)

const (
	dibRGBColors  = 0
	biRGB         = 0
	srccopy       = 0x00CC0020
	captureBLT    = 0x40000000
	bmpHeaderSize = 14
	dibHeaderSize = 40
)

type bitmapInfoHeader struct {
	Size            uint32
	Width           int32
	Height          int32
	Planes          uint16
	BitCount        uint16
	Compression     uint32
	SizeImage       uint32
	XPelsPerMeter   int32
	YPelsPerMeter   int32
	ColorsUsed      uint32
	ColorsImportant uint32
}

type bitmapInfo struct {
	Header bitmapInfoHeader
	Colors [1]uint32
}

var gdi32 = windows.NewLazySystemDLL("gdi32.dll")

func captureVirtualScreenBMP(path string, screen config.VirtualScreen) error {
	if screen.Width <= 0 || screen.Height <= 0 {
		return fmt.Errorf("invalid virtual-screen dimensions %dx%d", screen.Width, screen.Height)
	}
	pixelBytes := int64(screen.Width) * int64(screen.Height) * 4
	if pixelBytes > math.MaxUint32-bmpHeaderSize-dibHeaderSize {
		return fmt.Errorf("virtual-screen bitmap is too large: %dx%d", screen.Width, screen.Height)
	}

	getDC := user32.NewProc("GetDC")
	releaseDC := user32.NewProc("ReleaseDC")
	createCompatibleDC := gdi32.NewProc("CreateCompatibleDC")
	deleteDC := gdi32.NewProc("DeleteDC")
	createDIBSection := gdi32.NewProc("CreateDIBSection")
	selectObject := gdi32.NewProc("SelectObject")
	deleteObject := gdi32.NewProc("DeleteObject")
	bitBlt := gdi32.NewProc("BitBlt")

	desktopDC, _, callErr := getDC.Call(0)
	if desktopDC == 0 {
		return fmt.Errorf("GetDC for desktop failed: %v", callErr)
	}
	defer releaseDC.Call(0, desktopDC)

	memoryDC, _, callErr := createCompatibleDC.Call(desktopDC)
	if memoryDC == 0 {
		return fmt.Errorf("CreateCompatibleDC failed: %v", callErr)
	}
	defer deleteDC.Call(memoryDC)

	info := bitmapInfo{
		Header: bitmapInfoHeader{
			Size:  uint32(unsafe.Sizeof(bitmapInfoHeader{})),
			Width: int32(screen.Width), Height: -int32(screen.Height),
			Planes: 1, BitCount: 32, Compression: biRGB,
			SizeImage: uint32(pixelBytes),
		},
	}
	var pixels unsafe.Pointer
	bitmap, _, callErr := createDIBSection.Call(
		desktopDC, uintptr(unsafe.Pointer(&info)), dibRGBColors,
		uintptr(unsafe.Pointer(&pixels)), 0, 0,
	)
	if bitmap == 0 || pixels == nil {
		return fmt.Errorf("CreateDIBSection failed: %v", callErr)
	}
	defer deleteObject.Call(bitmap)
	oldObject, _, callErr := selectObject.Call(memoryDC, bitmap)
	if oldObject == 0 {
		return fmt.Errorf("SelectObject for screen bitmap failed: %v", callErr)
	}
	defer selectObject.Call(memoryDC, oldObject)
	copied, _, callErr := bitBlt.Call(
		memoryDC, 0, 0, uintptr(screen.Width), uintptr(screen.Height),
		desktopDC, uintptr(screen.X), uintptr(screen.Y), srccopy|captureBLT,
	)
	if copied == 0 {
		return fmt.Errorf("BitBlt virtual screen (%d,%d %dx%d) failed: %v", screen.X, screen.Y, screen.Width, screen.Height, callErr)
	}

	file, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create virtual-screen capture: %w", err)
	}
	defer file.Close()

	var headers [bmpHeaderSize + dibHeaderSize]byte
	headers[0], headers[1] = 'B', 'M'
	binary.LittleEndian.PutUint32(headers[2:6], uint32(len(headers))+uint32(pixelBytes))
	binary.LittleEndian.PutUint32(headers[10:14], uint32(len(headers)))
	binary.LittleEndian.PutUint32(headers[14:18], dibHeaderSize)
	binary.LittleEndian.PutUint32(headers[18:22], uint32(screen.Width))
	binary.LittleEndian.PutUint32(headers[22:26], uint32(int32(-screen.Height)))
	binary.LittleEndian.PutUint16(headers[26:28], 1)
	binary.LittleEndian.PutUint16(headers[28:30], 32)
	binary.LittleEndian.PutUint32(headers[34:38], uint32(pixelBytes))
	if _, err := file.Write(headers[:]); err != nil {
		return fmt.Errorf("write virtual-screen bitmap header: %w", err)
	}
	if _, err := file.Write(unsafe.Slice((*byte)(pixels), int(pixelBytes))); err != nil {
		return fmt.Errorf("write virtual-screen pixels: %w", err)
	}
	runtime.KeepAlive(&info)
	if err := file.Close(); err != nil {
		return fmt.Errorf("close virtual-screen bitmap: %w", err)
	}
	return nil
}
