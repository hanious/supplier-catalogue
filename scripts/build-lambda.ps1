$ErrorActionPreference = 'Stop'
$projectRoot = Split-Path -Parent $PSScriptRoot
Push-Location $projectRoot
$previousGOOS = $env:GOOS
$previousGOARCH = $env:GOARCH
$previousCGO = $env:CGO_ENABLED
try {
    New-Item -ItemType Directory -Force -Path dist | Out-Null
    $env:GOOS = 'linux'
    $env:GOARCH = 'arm64'
    $env:CGO_ENABLED = '0'
    go build -tags lambda.norpc -trimpath '-ldflags=-s -w' -o dist/bootstrap ./cmd/catalogue
    if ($LASTEXITCODE -ne 0) { throw 'Lambda compilation failed' }
    $env:GOOS = $previousGOOS
    $env:GOARCH = $previousGOARCH
    $env:CGO_ENABLED = $previousCGO
    # AWS's packager sets the ZIP creator platform and POSIX executable bits.
    go run github.com/aws/aws-lambda-go/cmd/build-lambda-zip -o dist/lambda.zip dist/bootstrap
    if ($LASTEXITCODE -ne 0) { throw 'Lambda packaging failed' }
    Write-Output 'Built dist/lambda.zip (Linux ARM64 bootstrap, executable permissions)'
} finally {
    $env:GOOS = $previousGOOS
    $env:GOARCH = $previousGOARCH
    $env:CGO_ENABLED = $previousCGO
    Pop-Location
}
