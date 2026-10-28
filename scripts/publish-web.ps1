param([string]$OutputsFile = 'dist/outputs.json')
$ErrorActionPreference = 'Stop'
$projectRoot = Split-Path -Parent $PSScriptRoot
Push-Location $projectRoot
try {
    $outputs = Get-Content -Raw -LiteralPath $OutputsFile | ConvertFrom-Json
    $service = $outputs.CatalogueServices
    if (-not $service.APIURL -or -not $service.AmplifyAppID) { throw 'CDK outputs must include APIURL and AmplifyAppID' }
    node scripts/package-web.mjs $service.APIURL
    if ($LASTEXITCODE -ne 0) { throw 'Static packaging failed' }
    Add-Type -AssemblyName System.IO.Compression.FileSystem
    $zipPath = Join-Path $projectRoot ('dist/web-' + [guid]::NewGuid().ToString('N') + '.zip')
    [System.IO.Compression.ZipFile]::CreateFromDirectory((Join-Path $projectRoot 'dist/web'), $zipPath)
    $rawDeployment = aws amplify create-deployment --app-id $service.AmplifyAppID --branch-name demo --output json
    if ($LASTEXITCODE -ne 0) { throw 'Amplify deployment creation failed' }
    $deployment = $rawDeployment | ConvertFrom-Json
    # The upload URL is temporary and must not be printed or committed.
    Invoke-WebRequest -Uri $deployment.zipUploadUrl -Method Put -InFile $zipPath -ContentType application/zip -UseBasicParsing | Out-Null
    $rawStarted = aws amplify start-deployment --app-id $service.AmplifyAppID --branch-name demo --job-id $deployment.jobId --output json
    if ($LASTEXITCODE -ne 0) { throw 'Amplify deployment start failed' }
    $started = $rawStarted | ConvertFrom-Json
    Write-Output ('Amplify job ' + $started.jobSummary.jobId + ' started. Check job status, then open ' + $service.FrontendURL)
} finally { Pop-Location }
