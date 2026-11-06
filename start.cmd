@echo off
rem DBBak local dev launcher - double-click friendly wrapper around scripts\start-dev.ps1
rem Usage: start.cmd [-Port 38080] [-Build] [-Restart] [-NoBrowser]
setlocal
cd /d "%~dp0"
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0scripts\start-dev.ps1" %*
set "CODE=%ERRORLEVEL%"
if not "%CODE%"=="0" (
    echo.
    echo [dbbak] startup failed, exit code %CODE%
    pause
)
exit /b %CODE%
