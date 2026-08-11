# Builds claudecost.exe (CLI) and claudecost-app.exe (windowed app).
# Run from the project root in PowerShell: .\build.ps1
# First run needs internet access (go mod tidy + go-winres download).
$ErrorActionPreference = "Stop"
if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    Write-Host "Go is not installed. Install it first: winget install GoLang.Go, then re-run."
    exit 1
}
Set-Location $PSScriptRoot
$goWinres = "go-winres"
if (-not (Get-Command go-winres -ErrorAction SilentlyContinue)) {
    $gopathBin = Join-Path (go env GOPATH) "bin\go-winres.exe"
    if (-not (Test-Path $gopathBin)) {
        Write-Host "Installing go-winres (one-time)..."
        go install github.com/tc-hib/go-winres@latest
    }
    $goWinres = $gopathBin
}
& $goWinres make --in winres\cli.json --out cmd\claudecost\rsrc
if ($LASTEXITCODE -ne 0) { Write-Host "go-winres failed for CLI; building without icon." }
& $goWinres make --in winres\app.json --out cmd\claudecost-app\rsrc
if ($LASTEXITCODE -ne 0) { Write-Host "go-winres failed for app; building without icon." }
go mod tidy
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
$env:GOOS = "windows"; $env:GOARCH = "amd64"; $env:CGO_ENABLED = "0"
go build -trimpath -ldflags "-s -w" -o claudecost.exe .\cmd\claudecost
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
go build -trimpath -ldflags "-s -w -H windowsgui" -o claudecost-app.exe .\cmd\claudecost-app
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
Write-Host "Built claudecost.exe and claudecost-app.exe"
