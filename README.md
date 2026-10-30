# PointRunner

PointRunner is a Windows-only, multi-point click runner written in Go. It runs in the system tray and exposes settings at `http://127.0.0.1:8765`.

> **Testing only.** Automated input may violate online-game terms of service or trigger anti-cheat systems. Do not use PointRunner in online games or to bypass anti-cheat protections. The application is intended for testing your own desktop software and repetitive local tasks.

## Features

- Sequential screen points with before/after delays, left/right/double clicks, and per-point repeats.
- List playback once, a fixed number of iterations, or infinitely; optional hold-to-run mode.
- **Smooth movement mode**: Gradually move cursor to target over configurable duration (500-3000ms) with human-like random deviations and jitter. Final click executed at destination. Old instant-click mode remains default.
- Global low-level keyboard hook: `Ctrl+Alt+H` starts/stops playback, `Ctrl+Alt+P` picks a screen point, and `Ctrl+Alt+Esc` is the fixed emergency stop.
- Multi-monitor point picking and validation over the virtual desktop.
- Optional text targets search the foreground window's accessibility tree first (including browser page text), with full virtual-desktop OCR as a fallback.
- Reorder or delete steps; save, load, and delete JSON profiles.
- Tray controls for start/stop, pause/resume, and opening settings.
- `SendInput` absolute mouse input and a 1 ms Windows timer-period request.

## Build and test

Install Go 1.22 or newer, then run in PowerShell:

```powershell
.\build.ps1
go test ./...
```

The build produces `pointrunner.exe` without a console window. No CGO is required.

## Use

Launch `pointrunner.exe`; open its settings from the tray menu at `http://127.0.0.1:8765`. Add points manually or choose **Pick exact point** and click the desired screen coordinate. Edit each point's before/after delays (zero is allowed), click type, and repeats. Use **Move up/Move down** to change the playback order, and **Delete step** to remove a point. Save settings when finished. Profiles can be saved, loaded, and deleted in the Profiles controls.

To click text that moves on the screen, add a point and fill **Click text in active window** with the phrase to match (for example, `Slow download`). Keep the target application or browser page in the foreground while playback runs. PointRunner first searches the foreground window's Windows UI Automation accessibility tree and clicks the center of a matching text element; this can find page text such as `<span>Slow download</span>` even when OCR cannot read the rendered pixels. If the accessibility tree has no match, it captures the entire virtual desktop in physical pixels and falls back to Windows OCR. Secondary displays with negative or offset coordinates are included; per-monitor DPI awareness keeps OCR coordinates and clicks in the same coordinate system. Install a Windows OCR language pack for the language displayed on screen. If the target is not found, playback stops and logs the accessibility-tree result or OCR screen bounds and recognized text. Keep **Text** empty to click the saved X/Y coordinates instead.

Playback and picker hotkeys are configurable in settings. In hold mode, playback continues only while the configured playback hotkey is held. `Ctrl+Alt+Esc` stops playback regardless of the configured hotkey.

Configuration and diagnostics are stored under `%APPDATA%\pointrunner\`:

- `config.json` — active settings
- `profiles\*.json` — saved profiles
- `log.txt` — timestamped startup/shutdown, hotkey, tray, configuration/profile, playback-step, click, HTTP API, and WinAPI diagnostics. Text searches log the accessibility-tree match/window/coordinates or OCR target, virtual-screen dimensions, elapsed time, every recognized line with its screen coordinates, and whether/how it matched. The active log is capped at 5 MB with three rotated backups (`log.txt.1` through `log.txt.3`).
- `debug\last-ocr-failure.bmp` — overwritten with the most recent full virtual-desktop capture when an OCR search fails. The image may contain sensitive screen content; inspect and delete it when no longer needed. Successful OCR searches do not retain a screenshot.

The HTTP server binds only to loopback. Keep the application private to your local machine.

OCR recognizes text, not the appearance of buttons or images. If the target is an image or OCR cannot read its label, use a fixed screen point; the failure screenshot and complete OCR lines in `log.txt` can help diagnose what Windows recognized.

## License

MIT; see [LICENSE](LICENSE).
