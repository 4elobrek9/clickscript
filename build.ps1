$ErrorActionPreference = "Stop"

if ($env:GOOS -and $env:GOOS -ne "windows") {
    throw "PointRunner can only be built for Windows."
}

$env:GOOS = "windows"
$env:CGO_ENABLED = "0"
go build -ldflags="-H windowsgui" -o pointrunner.exe ./cmd/pointrunner
