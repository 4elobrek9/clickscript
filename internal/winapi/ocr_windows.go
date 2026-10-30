//go:build windows

package winapi

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf16"
)

const powershellOCR = `$ErrorActionPreference = 'Stop'
trap { [Console]::Out.WriteLine('POINTRUNNER_ERROR:' + $_.ToString()); [Console]::Out.WriteLine($_.ScriptStackTrace); exit 1 }
[Console]::OutputEncoding = New-Object System.Text.UTF8Encoding($false)
Add-Type -AssemblyName System.Drawing
Add-Type -AssemblyName System.Runtime.WindowsRuntime
[Windows.Storage.StorageFile, Windows.Storage, ContentType=WindowsRuntime] | Out-Null
[Windows.Graphics.Imaging.BitmapDecoder, Windows.Graphics.Imaging, ContentType=WindowsRuntime] | Out-Null
[Windows.Media.Ocr.OcrEngine, Windows.Media.Ocr, ContentType=WindowsRuntime] | Out-Null
[Windows.Storage.Streams.IRandomAccessStream, Windows.Storage, ContentType=WindowsRuntime] | Out-Null
$asyncMethod = [System.WindowsRuntimeSystemExtensions].GetMethods() | Where-Object {
  $_.Name -eq 'AsTask' -and $_.GetParameters().Count -eq 1 -and $_.GetParameters()[0].ParameterType.Name -eq ('IAsyncOperation' + [char]96 + '1')
} | Select-Object -First 1
if (-not $asyncMethod) { throw 'Windows OCR async bridge is unavailable' }
function Wait-WinRT($operation, [Type]$resultType) {
  $task = $asyncMethod.MakeGenericMethod($resultType).Invoke($null, @($operation))
  $task.Wait()
  return $task.Result
}
$engine = [Windows.Media.Ocr.OcrEngine]::TryCreateFromUserProfileLanguages()
if (-not $engine) { throw 'Windows OCR is unavailable; install an OCR language pack in Windows Settings' }
$file = Wait-WinRT ([Windows.Storage.StorageFile]::GetFileFromPathAsync($env:POINTRUNNER_CAPTURE_PATH)) ([Windows.Storage.StorageFile])
$stream = Wait-WinRT ($file.OpenAsync([Windows.Storage.FileAccessMode]::Read)) ([Windows.Storage.Streams.IRandomAccessStream])
$decoder = Wait-WinRT ([Windows.Graphics.Imaging.BitmapDecoder]::CreateAsync($stream)) ([Windows.Graphics.Imaging.BitmapDecoder])
$softwareBitmap = Wait-WinRT ($decoder.GetSoftwareBitmapAsync()) ([Windows.Graphics.Imaging.SoftwareBitmap])
$result = Wait-WinRT ($engine.RecognizeAsync($softwareBitmap)) ([Windows.Media.Ocr.OcrResult])
$stream.Dispose()
$lines = @()
foreach ($line in $result.Lines) {
  if ($line.Words.Count -eq 0) { continue }
  $left = [double]::PositiveInfinity; $top = [double]::PositiveInfinity
  $right = 0.0; $bottom = 0.0
  foreach ($word in $line.Words) {
    $rect = $word.BoundingRect
    $left = [Math]::Min($left, $rect.X); $top = [Math]::Min($top, $rect.Y)
    $right = [Math]::Max($right, $rect.X + $rect.Width)
    $bottom = [Math]::Max($bottom, $rect.Y + $rect.Height)
  }
  $lines += [PSCustomObject]@{ text = $line.Text; x = [int]$left; y = [int]$top; width = [int]($right - $left); height = [int]($bottom - $top) }
}
ConvertTo-Json -InputObject ([PSCustomObject]@{ lines = @($lines) }) -Compress -Depth 4
`

const powershellUIATextSearch = `$ErrorActionPreference = 'Stop'
[Console]::OutputEncoding = New-Object System.Text.UTF8Encoding($false)
try {
  Add-Type -AssemblyName UIAutomationClient
  $handle = [IntPtr]([long]$env:POINTRUNNER_WINDOW_HANDLE)
  $root = [System.Windows.Automation.AutomationElement]::FromHandle($handle)
  if (-not $root) { throw 'Could not access the foreground window accessibility tree' }
  $needle = [System.Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($env:POINTRUNNER_TEXT_TARGET))
  $normalize = { param($value) ([regex]::Replace($value.ToLowerInvariant(), '[^\p{L}\p{Nd}]+', ' ')).Trim() }
  $normalizedNeedle = & $normalize $needle
  $nodes = $root.FindAll(
    [System.Windows.Automation.TreeScope]::Descendants,
    [System.Windows.Automation.Condition]::TrueCondition
  )
  $best = $null
  $readErrors = 0
  foreach ($node in $nodes) {
    try {
      $name = $node.Current.Name
      if ([string]::IsNullOrWhiteSpace($name)) { continue }
      $normalizedName = & $normalize $name
      if ($normalizedName -ne $normalizedNeedle) { continue }
      $rect = $node.Current.BoundingRectangle
      if ($rect.IsEmpty -or $rect.Width -le 0 -or $rect.Height -le 0) { continue }
      if (-not $best) {
        $best = [PSCustomObject]@{
          found = $true
          text = $name
          x = [int][Math]::Round($rect.X)
          y = [int][Math]::Round($rect.Y)
          width = [int][Math]::Round($rect.Width)
          height = [int][Math]::Round($rect.Height)
          exact = $true
          window = $root.Current.Name
          elements = $nodes.Count
          read_errors = $readErrors
        }
        break
      }
    } catch { $readErrors++ }
  }
  if ($best) {
    ConvertTo-Json -InputObject $best -Compress
  } else {
    ConvertTo-Json -InputObject ([PSCustomObject]@{
      found = $false
      window = $root.Current.Name
      elements = $nodes.Count
      read_errors = $readErrors
    }) -Compress
  }
} catch {
  ConvertTo-Json -InputObject ([PSCustomObject]@{ found = $false; error = $_.Exception.Message }) -Compress
}
`

type ocrResponse struct {
	Lines []ocrLine `json:"lines"`
}

type uiAutomationTextResult struct {
	Found      bool   `json:"found"`
	Text       string `json:"text"`
	X          int    `json:"x"`
	Y          int    `json:"y"`
	Width      int    `json:"width"`
	Height     int    `json:"height"`
	Exact      bool   `json:"exact"`
	Window     string `json:"window"`
	Elements   int    `json:"elements"`
	Error      string `json:"error"`
	ReadErrors int    `json:"read_errors"`
}

type ocrLine struct {
	Text   string `json:"text"`
	X      int    `json:"x"`
	Y      int    `json:"y"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

func FindText(parent context.Context, target string) (int, int, error) {
	return FindTextWithDiagnostics(parent, target, nil, "")
}

func FindTextWithDiagnostics(parent context.Context, target string, logf func(string, ...any), debugDir string) (int, int, error) {
	started := time.Now()
	diagnosticLog(logf, "text target search started: target=%q normalized=%q", target, normalizeOCRText(target))
	if normalizeOCRText(target) == "" {
		diagnosticLog(logf, "OCR search rejected empty target")
		return 0, 0, fmt.Errorf("text target cannot be empty")
	}
	if x, y, result, err := findTextInForegroundUIA(parent, target); err == nil {
		if result.Found {
			diagnosticLog(logf, "UI Automation matched foreground-window text: target=%q element=%q exact=%t window=%q elements_scanned=%d read_errors=%d bounds=(%d,%d %dx%d) click=(%d,%d) elapsed=%s",
				target, result.Text, result.Exact, result.Window, result.Elements, result.ReadErrors, result.X, result.Y,
				result.Width, result.Height, x, y, time.Since(started))
			return x, y, nil
		}
		diagnosticLog(logf, "UI Automation found no matching text in foreground window=%q elements_scanned=%d read_errors=%d; falling back to OCR",
			result.Window, result.Elements, result.ReadErrors)
		if result.Error != "" {
			diagnosticLog(logf, "UI Automation detail: %s", result.Error)
		}
	} else {
		diagnosticLog(logf, "UI Automation search failed; falling back to OCR: %v", err)
	}
	ctx, cancel := context.WithTimeout(parent, 12*time.Second)
	defer cancel()
	screen, err := VirtualScreen()
	if err != nil {
		diagnosticLog(logf, "OCR failed to get virtual-screen dimensions: %v", err)
		return 0, 0, fmt.Errorf("get virtual screen for OCR: %w", err)
	}
	diagnosticLog(logf, "OCR virtual screen: origin=(%d,%d) dimensions=%dx%d", screen.X, screen.Y, screen.Width, screen.Height)
	capture, err := os.CreateTemp("", "pointrunner-screen-*.bmp")
	if err != nil {
		diagnosticLog(logf, "OCR could not create temporary capture: %v", err)
		return 0, 0, fmt.Errorf("create OCR capture file: %w", err)
	}
	capturePath := capture.Name()
	if err := capture.Close(); err != nil {
		os.Remove(capturePath)
		diagnosticLog(logf, "OCR could not close temporary capture file: %v", err)
		return 0, 0, fmt.Errorf("close temporary OCR capture file: %w", err)
	}
	defer os.Remove(capturePath)
	if err := captureVirtualScreenBMP(capturePath, screen); err != nil {
		diagnosticLog(logf, "OCR screen capture failed: %v", err)
		return 0, 0, fmt.Errorf("capture virtual screen for OCR: %w", err)
	}
	diagnosticLog(logf, "OCR captured full virtual screen: path=%q elapsed=%s", capturePath, time.Since(started))
	success := false
	defer func() {
		if !success && debugDir != "" {
			debugPath, saveErr := saveFailedOCRCapture(capturePath, debugDir)
			if saveErr != nil {
				diagnosticLog(logf, "OCR could not preserve failed capture: %v", saveErr)
			} else {
				diagnosticLog(logf, "OCR failed-screen capture saved to %q", debugPath)
			}
		}
	}()
	encoded := base64.StdEncoding.EncodeToString([]byte(utf16LE(powershellOCR)))
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-WindowStyle", "Hidden", "-EncodedCommand", encoded)
	cmd.SysProcAttr = windowsProcAttrs()
	cmd.Env = append(os.Environ(), "POINTRUNNER_CAPTURE_PATH="+capturePath)
	output, err := cmd.Output()
	if parent.Err() != nil {
		diagnosticLog(logf, "OCR search cancelled after %s: %v", time.Since(started), parent.Err())
		return 0, 0, parent.Err()
	}
	if ctx.Err() != nil {
		diagnosticLog(logf, "OCR process timed out after %s", time.Since(started))
		return 0, 0, fmt.Errorf("screen text recognition timed out after 12 seconds")
	}
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			if details := strings.TrimSpace(string(output)); strings.HasPrefix(details, "POINTRUNNER_ERROR:") {
				diagnosticLog(logf, "OCR PowerShell/WinRT failed after %s: %s", time.Since(started), details)
				return 0, 0, fmt.Errorf("screen text recognition failed: %s", strings.TrimSpace(strings.TrimPrefix(details, "POINTRUNNER_ERROR:")))
			}
			diagnosticLog(logf, "OCR PowerShell process failed after %s: stderr=%q output=%q", time.Since(started), strings.TrimSpace(string(exitErr.Stderr)), strings.TrimSpace(string(output)))
			return 0, 0, fmt.Errorf("screen text recognition failed: %s", strings.TrimSpace(string(exitErr.Stderr)))
		}
		diagnosticLog(logf, "starting OCR process failed after %s: %v", time.Since(started), err)
		return 0, 0, fmt.Errorf("start Windows OCR: %w", err)
	}
	var response ocrResponse
	if err := json.Unmarshal(output, &response); err != nil {
		diagnosticLog(logf, "OCR result JSON parse failed after %s: error=%v output=%q", time.Since(started), err, strings.TrimSpace(string(output)))
		return 0, 0, fmt.Errorf("parse Windows OCR result: %w", err)
	}
	diagnosticLog(logf, "OCR completed: elapsed=%s lines=%d target=%q", time.Since(started), len(response.Lines), target)
	for i, line := range response.Lines {
		diagnosticLog(logf, "OCR line[%d]: text=%q normalized=%q bounds=(%d,%d %dx%d) screen=(%d,%d)",
			i+1, line.Text, normalizeOCRText(line.Text), line.X, line.Y, line.Width, line.Height,
			screen.X+line.X, screen.Y+line.Y)
	}
	if match, ok := matchOCRLines(target, response.Lines); ok {
		x, y := screen.X+match.X+match.Width/2, screen.Y+match.Y+match.Height/2
		success = true
		diagnosticLog(logf, "OCR target matched: target=%q bounds=(%d,%d %dx%d) click=(%d,%d) elapsed=%s",
			target, match.X, match.Y, match.Width, match.Height, x, y, time.Since(started))
		return x, y, nil
	}
	summary := summarizeOCRLines(response.Lines)
	diagnosticLog(logf, "OCR target NOT found: target=%q lines=%d recognized_summary=%s elapsed=%s",
		target, len(response.Lines), summary, time.Since(started))
	return 0, 0, fmt.Errorf("text %q was not found across %d OCR lines on virtual desktop (%d,%d %dx%d); recognized: %s",
		target, len(response.Lines), screen.X, screen.Y, screen.Width, screen.Height, summary)
}

func findTextInForegroundUIA(parent context.Context, target string) (int, int, uiAutomationTextResult, error) {
	hwnd, _, callErr := user32.NewProc("GetForegroundWindow").Call()
	if hwnd == 0 {
		return 0, 0, uiAutomationTextResult{}, fmt.Errorf("GetForegroundWindow failed: %v", callErr)
	}
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()
	encoded := base64.StdEncoding.EncodeToString([]byte(utf16LE(powershellUIATextSearch)))
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-WindowStyle", "Hidden", "-EncodedCommand", encoded)
	cmd.SysProcAttr = windowsProcAttrs()
	cmd.Env = append(os.Environ(),
		"POINTRUNNER_WINDOW_HANDLE="+strconv.FormatUint(uint64(hwnd), 10),
		"POINTRUNNER_TEXT_TARGET="+base64.StdEncoding.EncodeToString([]byte(target)),
	)
	output, err := cmd.Output()
	if ctx.Err() != nil {
		return 0, 0, uiAutomationTextResult{}, fmt.Errorf("UI Automation search timed out after 3 seconds")
	}
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return 0, 0, uiAutomationTextResult{}, fmt.Errorf("PowerShell UI Automation process failed: stderr=%q output=%q",
				strings.TrimSpace(string(exitErr.Stderr)), strings.TrimSpace(string(output)))
		}
		return 0, 0, uiAutomationTextResult{}, fmt.Errorf("start PowerShell UI Automation: %w", err)
	}
	var result uiAutomationTextResult
	if err := json.Unmarshal(output, &result); err != nil {
		return 0, 0, uiAutomationTextResult{}, fmt.Errorf("parse UI Automation result: %w (output=%q)", err, strings.TrimSpace(string(output)))
	}
	if !result.Found {
		return 0, 0, result, nil
	}
	return result.X + result.Width/2, result.Y + result.Height/2, result, nil
}

func diagnosticLog(logf func(string, ...any), format string, args ...any) {
	if logf != nil {
		logf(format, args...)
	}
}

func saveFailedOCRCapture(source, debugDir string) (string, error) {
	if err := os.MkdirAll(debugDir, 0700); err != nil {
		return "", fmt.Errorf("create OCR debug directory: %w", err)
	}
	target := filepath.Join(debugDir, "last-ocr-failure.bmp")
	data, err := os.ReadFile(source)
	if err != nil {
		return "", fmt.Errorf("read failed OCR capture: %w", err)
	}
	if err := os.WriteFile(target, data, 0600); err != nil {
		return "", fmt.Errorf("save failed OCR capture: %w", err)
	}
	return target, nil
}

func matchOCRLines(target string, lines []ocrLine) (ocrLine, bool) {
	needle := normalizeOCRText(target)
	if needle == "" {
		return ocrLine{}, false
	}
	type normalizedLine struct {
		line  ocrLine
		start int
		end   int
	}
	var normalized []normalizedLine
	var joined strings.Builder
	for _, line := range lines {
		text := normalizeOCRText(line.Text)
		if text == "" {
			continue
		}
		if joined.Len() > 0 {
			joined.WriteByte(' ')
		}
		start := joined.Len()
		joined.WriteString(text)
		normalized = append(normalized, normalizedLine{line: line, start: start, end: joined.Len()})
	}
	matchStart := strings.Index(joined.String(), needle)
	if matchStart < 0 {
		return ocrLine{}, false
	}
	matchEnd := matchStart + len(needle)
	var combined ocrLine
	found := false
	for _, item := range normalized {
		if item.start >= matchEnd || item.end <= matchStart {
			continue
		}
		right := item.line.X + item.line.Width
		bottom := item.line.Y + item.line.Height
		if !found {
			combined = item.line
			found = true
			continue
		}
		left := min(combined.X, item.line.X)
		top := min(combined.Y, item.line.Y)
		right = max(combined.X+combined.Width, right)
		bottom = max(combined.Y+combined.Height, bottom)
		combined.X, combined.Y = left, top
		combined.Width, combined.Height = right-left, bottom-top
	}
	return combined, found
}

func summarizeOCRLines(lines []ocrLine) string {
	const maxLines = 8
	const maxChars = 80
	if len(lines) == 0 {
		return "(OCR returned no text)"
	}
	parts := make([]string, 0, min(len(lines), maxLines))
	for _, line := range lines[:min(len(lines), maxLines)] {
		text := strings.TrimSpace(line.Text)
		if len(text) > maxChars {
			text = text[:maxChars] + "..."
		}
		parts = append(parts, fmt.Sprintf("%q", text))
	}
	if len(lines) > maxLines {
		parts = append(parts, fmt.Sprintf("... and %d more", len(lines)-maxLines))
	}
	return strings.Join(parts, "; ")
}

func normalizeOCRText(value string) string {
	var out strings.Builder
	space := false
	for _, r := range strings.ToLower(value) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			out.WriteRune(r)
			space = false
		} else if !space && out.Len() > 0 {
			out.WriteByte(' ')
			space = true
		}
	}
	return strings.TrimSpace(out.String())
}

func utf16LE(value string) []byte {
	units := utf16.Encode([]rune(value))
	encoded := make([]byte, len(units)*2)
	for i, unit := range units {
		binary.LittleEndian.PutUint16(encoded[i*2:], unit)
	}
	return encoded
}

func windowsProcAttrs() *syscall.SysProcAttr { return &syscall.SysProcAttr{HideWindow: true} }
