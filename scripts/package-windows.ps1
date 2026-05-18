$ErrorActionPreference = "Stop"
$Root = Resolve-Path "$PSScriptRoot\.."
Set-Location $Root
New-Item -ItemType Directory -Force -Path dist | Out-Null
$env:GOOS="windows"
$env:GOARCH="amd64"
go build -o dist\ClusterKit.exe .\cmd\clusterkit
@'
@echo off
"%~dp0ClusterKit.exe"
pause
'@ | Set-Content -Encoding ASCII dist\start.bat
@'
@echo off
"%~dp0ClusterKit.exe"
pause
'@ | Set-Content -Encoding ASCII dist\terminal-ui.bat
@'
@echo off
start "ClusterKit Web" "%~dp0ClusterKit.exe" --web
'@ | Set-Content -Encoding ASCII dist\web-ui.bat
@'
@echo off
echo ClusterKit is portable. No installation required.
echo Run start.bat or ClusterKit.exe.
pause
'@ | Set-Content -Encoding ASCII dist\install.bat
if (Test-Path dist\ClusterKit-windows-amd64.zip) { Remove-Item dist\ClusterKit-windows-amd64.zip }
Compress-Archive -Path dist\ClusterKit.exe,dist\start.bat,dist\terminal-ui.bat,dist\web-ui.bat,dist\install.bat -DestinationPath dist\ClusterKit-windows-amd64.zip
Write-Host "Built: $Root\dist\ClusterKit-windows-amd64.zip"
