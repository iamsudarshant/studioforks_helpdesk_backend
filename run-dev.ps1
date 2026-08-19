<#
.SYNOPSIS
    Starts the ComplyDesk backend and frontend together for local development.

.DESCRIPTION
    Checks the prerequisites, launches the Go API and the Vite dev server in
    their own windows, waits until each answers, and prints where to go.

    The frontend calls a relative /api/v1 which Vite proxies to the backend, so
    there is no CORS involved and only one URL to open.

.PARAMETER ApiPort
    Port for the Go API. Defaults to 8090 because XAMPP Apache usually holds 8080.

.PARAMETER Reseed
    Drop nothing, but re-run migrations and the demo seed before starting.

.PARAMETER BackendOnly
    Start only the API.

.EXAMPLE
    .\run-dev.ps1
    .\run-dev.ps1 -Reseed
    .\run-dev.ps1 -ApiPort 9000
#>

[CmdletBinding()]
param(
    [int]$ApiPort = 8090,
    [switch]$Reseed,
    [switch]$BackendOnly
)

$ErrorActionPreference = 'Stop'

$BackendDir  = $PSScriptRoot
$FrontendDir = Join-Path (Split-Path $BackendDir -Parent) 'complydesk_frontend'

function Write-Step { param([string]$Message) Write-Host "==> $Message" -ForegroundColor Cyan }
function Write-Ok   { param([string]$Message) Write-Host "    $Message" -ForegroundColor Green }
function Write-Warn { param([string]$Message) Write-Host "    $Message" -ForegroundColor Yellow }

function Test-Port {
    param([int]$Port)
    $listener = Get-NetTCPConnection -LocalPort $Port -State Listen -ErrorAction SilentlyContinue
    return $null -ne $listener
}

function Wait-ForUrl {
    param([string]$Url, [int]$TimeoutSeconds = 45)
    $deadline = (Get-Date).AddSeconds($TimeoutSeconds)
    while ((Get-Date) -lt $deadline) {
        try {
            $response = Invoke-WebRequest -Uri $Url -UseBasicParsing -TimeoutSec 3
            if ($response.StatusCode -eq 200) { return $true }
        } catch {
            Start-Sleep -Milliseconds 700
        }
    }
    return $false
}

# --------------------------------------------------------------- prerequisites

Write-Step 'Checking prerequisites'

if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    throw 'Go is not on PATH. Install Go 1.23+ and reopen the terminal.'
}
Write-Ok "Go $((go version) -replace 'go version ', '')"

if (-not $BackendOnly) {
    if (-not (Get-Command npm -ErrorAction SilentlyContinue)) {
        throw 'npm is not on PATH. Install Node 20+ and reopen the terminal.'
    }
    if (-not (Test-Path $FrontendDir)) {
        throw "Frontend not found at $FrontendDir"
    }
    if (-not (Test-Path (Join-Path $FrontendDir 'node_modules'))) {
        Write-Warn 'node_modules missing — running npm install (this takes a minute)'
        Push-Location $FrontendDir
        try { npm install } finally { Pop-Location }
    }
    Write-Ok "Node $(node --version)"
}

# MariaDB is the one dependency with no graceful degradation.
if (-not (Test-Port -Port 3306)) {
    throw 'MariaDB is not listening on 3306. Start MySQL from the XAMPP control panel and try again.'
}
Write-Ok 'MariaDB is up on 3306'

# Redis is optional in development: the API logs a warning and turns off rate
# limiting and caching rather than refusing to start.
if (Test-Port -Port 6379) {
    Write-Ok 'Redis is up on 6379'
} else {
    Write-Warn 'Redis is not running — rate limiting and caching are disabled (fine for development)'
}

if (Test-Port -Port $ApiPort) {
    throw "Port $ApiPort is already in use. Stop whatever holds it, or pass -ApiPort with a free port."
}

if (-not (Test-Path (Join-Path $BackendDir '.env'))) {
    throw "No .env in $BackendDir. Copy .env.example to .env and set JWT_SECRET and MASTER_KEK."
}

# --------------------------------------------------------------- optional seed

if ($Reseed) {
    Write-Step 'Applying migrations and reseeding the demo workspace'
    Push-Location $BackendDir
    try {
        go run ./cmd/cli createdb
        go run ./cmd/cli migrate up
        go run ./cmd/cli seed --demo
    } finally {
        Pop-Location
    }
}

# --------------------------------------------------------------- backend

Write-Step "Starting the API on http://localhost:$ApiPort"

$apiEnv = "`$env:APP_PORT='$ApiPort'; `$env:APP_BASE_URL='http://localhost:$ApiPort';"
Start-Process powershell -ArgumentList @(
    '-NoExit', '-Command',
    "cd '$BackendDir'; $apiEnv Write-Host 'ComplyDesk API' -ForegroundColor Cyan; go run ./cmd/api"
) | Out-Null

if (Wait-ForUrl -Url "http://localhost:$ApiPort/api/v1/health") {
    Write-Ok "API is answering on http://localhost:$ApiPort/api/v1/health"
} else {
    Write-Warn 'API did not answer within 45s — check its window for the failure'
}

if ($BackendOnly) {
    Write-Host ''
    Write-Host "API: http://localhost:$ApiPort/api/v1" -ForegroundColor Green
    return
}

# --------------------------------------------------------------- frontend

Write-Step 'Starting the frontend on http://localhost:5173'

Start-Process powershell -ArgumentList @(
    '-NoExit', '-Command',
    "cd '$FrontendDir'; `$env:VITE_PROXY_TARGET='http://localhost:$ApiPort'; " +
    "Write-Host 'ComplyDesk frontend' -ForegroundColor Magenta; npm run dev"
) | Out-Null

if (Wait-ForUrl -Url 'http://localhost:5173' -TimeoutSeconds 60) {
    Write-Ok 'Frontend is serving on http://localhost:5173'
} else {
    Write-Warn 'Frontend did not answer within 60s — check its window for the failure'
}

# --------------------------------------------------------------- summary

Write-Host ''
Write-Host '  ComplyDesk is running' -ForegroundColor Green
Write-Host '  ---------------------'
Write-Host "  App        http://localhost:5173"
Write-Host "  API        http://localhost:$ApiPort/api/v1  (proxied via /api/v1)"
Write-Host ''
Write-Host '  Sign in with a seeded account, password ComplyDesk@2026:' -ForegroundColor Cyan
Write-Host '    /admin     master.admin@demo.local'
Write-Host '    /agents    helpdesk.admin@demo.local'
Write-Host '    /partner   client.admin@demo.local'
Write-Host '    /user      employee@demo.local'
Write-Host ''
Write-Host '  Auth, users, groups, roles and org structure are served by the API.'
Write-Host '  Tickets, dashboards, documents and reports are still mocked.'
Write-Host '  See docs/RUNNING.md for the full split.' -ForegroundColor DarkGray
Write-Host ''
