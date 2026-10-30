//go:build windows

package winapi

import (
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestNormalizeOCRText(t *testing.T) {
	got := normalizeOCRText("  ПОДТВЕРДИТЬ—Yes!  ")
	if got != "подтвердить yes" {
		t.Fatalf("normalizeOCRText() = %q", got)
	}
}

func TestMatchOCRLine(t *testing.T) {
	lines := []ocrLine{
		{Text: "Continue later", X: 10, Y: 20, Width: 60, Height: 12},
	}
	got, ok := matchOCRLines("CONTINUE", lines)
	if !ok || got.X != 10 || got.Y != 20 {
		t.Fatalf("matchOCRLine() = (%+v, %v)", got, ok)
	}
	if _, ok := matchOCRLines("Cancel", lines); ok {
		t.Fatal("matchOCRLines() unexpectedly matched missing text")
	}
}

func TestMatchOCRTextAcrossLines(t *testing.T) {
	lines := []ocrLine{
		{Text: "Confirm your", X: 10, Y: 10, Width: 70, Height: 12},
		{Text: "selection", X: 20, Y: 30, Width: 50, Height: 12},
	}
	got, ok := matchOCRLines("your selection", lines)
	if !ok || got.X != 10 || got.Y != 10 || got.Width != 70 || got.Height != 32 {
		t.Fatalf("cross-line text bounds = (%+v, %v)", got, ok)
	}
}

func TestFindTextOCRIntegration(t *testing.T) {
	if os.Getenv("POINTRUNNER_OCR_INTEGRATION") != "1" {
		t.Skip("set POINTRUNNER_OCR_INTEGRATION=1 to test Windows OCR and screenshot capture")
	}
	screen, err := VirtualScreen()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("OCR captures the virtual desktop at (%d,%d), size %dx%d", screen.X, screen.Y, screen.Width, screen.Height)
	_, _, err = FindText(context.Background(), "__pointrunner_text_not_on_screen_987654__")
	if err == nil || !strings.Contains(err.Error(), "was not found across") {
		t.Fatalf("expected OCR to process the screen and report no match, got %v", err)
	}
}

func TestFindTextOnSecondaryMonitorIntegration(t *testing.T) {
	if os.Getenv("POINTRUNNER_OCR_INTEGRATION") != "1" {
		t.Skip("set POINTRUNNER_OCR_INTEGRATION=1 to test Windows OCR on a secondary monitor")
	}
	screen, err := VirtualScreen()
	if err != nil {
		t.Fatal(err)
	}
	if screen.X >= 0 {
		t.Skip("no secondary monitor was found to the left of the primary display")
	}
	left, top := screen.X+100, screen.Y+100
	script := `$ErrorActionPreference='Stop'
Add-Type -TypeDefinition 'using System; using System.Runtime.InteropServices; public static class PointRunnerTestDpi { [DllImport("user32.dll")] public static extern bool SetProcessDpiAwarenessContext(IntPtr value); }'
[PointRunnerTestDpi]::SetProcessDpiAwarenessContext([IntPtr](-4)) | Out-Null
Add-Type -AssemblyName System.Windows.Forms
Add-Type -AssemblyName System.Drawing
$form=New-Object System.Windows.Forms.Form
$form.FormBorderStyle='None'; $form.StartPosition='Manual'; $form.Location=New-Object System.Drawing.Point(` + strconv.Itoa(left) + `,` + strconv.Itoa(top) + `)
$form.Size=New-Object System.Drawing.Size(800,180); $form.BackColor=[System.Drawing.Color]::White; $form.TopMost=$true
$label=New-Object System.Windows.Forms.Label
$label.Dock='Fill'; $label.TextAlign='MiddleCenter'; $label.Text='POINTRUNNER OCR TEST'; $label.ForeColor=[System.Drawing.Color]::Black
$label.Font=New-Object System.Drawing.Font('Arial',32,[System.Drawing.FontStyle]::Bold); $form.Controls.Add($label)
$form.ShowDialog() | Out-Null
`
	command := exec.Command("powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-WindowStyle", "Hidden", "-EncodedCommand", base64.StdEncoding.EncodeToString([]byte(utf16LE(script))))
	command.SysProcAttr = windowsProcAttrs()
	if err := command.Start(); err != nil {
		t.Fatalf("start secondary-monitor text fixture: %v", err)
	}
	defer func() {
		_ = command.Process.Kill()
		_ = command.Wait()
	}()
	time.Sleep(700 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	x, y, err := FindText(ctx, "POINTRUNNER OCR TEST")
	if err != nil {
		t.Fatalf("find known text on secondary monitor (%d,%d): %v", left, top, err)
	}
	if x < left || x >= left+800 || y < top || y >= top+180 {
		t.Fatalf("OCR returned (%d,%d), outside the secondary-monitor fixture at (%d,%d)", x, y, left, top)
	}
}
