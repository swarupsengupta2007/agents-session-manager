@echo off
setlocal EnableExtensions
cd /d "%~dp0"

rem Build agents-session-manager for the host, or cross-compile.
rem Usage: build.cmd [native|linux|windows|macos|all]

set "NAME=agents-session-manager"
if not defined OUT set "OUT=dist"
set "TARGET=%~1"
if "%TARGET%"=="" set "TARGET=native"

if /I "%TARGET%"=="-h" goto :help
if /I "%TARGET%"=="--help" goto :help
if /I "%TARGET%"=="help" goto :help

if /I "%TARGET%"=="native" goto :native
if /I "%TARGET%"=="linux" goto :linux
if /I "%TARGET%"=="windows" goto :windows
if /I "%TARGET%"=="macos" goto :macos
if /I "%TARGET%"=="darwin" goto :macos
if /I "%TARGET%"=="mac" goto :macos
if /I "%TARGET%"=="all" goto :all

echo unknown target: %TARGET% (try native^|linux^|windows^|macos^|all) 1>&2
exit /b 2

:native
for /f "delims=" %%i in ('go env GOOS') do set "GOOS_NATIVE=%%i"
for /f "delims=" %%i in ('go env GOARCH') do set "GOARCH_NATIVE=%%i"
call :build "%GOOS_NATIVE%" "%GOARCH_NATIVE%"
exit /b %ERRORLEVEL%

:linux
call :build linux amd64
if errorlevel 1 exit /b 1
call :build linux arm64
exit /b %ERRORLEVEL%

:windows
call :build windows amd64
if errorlevel 1 exit /b 1
call :build windows arm64
exit /b %ERRORLEVEL%

:macos
call :build darwin arm64
if errorlevel 1 exit /b 1
call :build darwin amd64
exit /b %ERRORLEVEL%

:all
call :build linux amd64
if errorlevel 1 exit /b 1
call :build linux arm64
if errorlevel 1 exit /b 1
call :build windows amd64
if errorlevel 1 exit /b 1
call :build windows arm64
if errorlevel 1 exit /b 1
call :build darwin arm64
if errorlevel 1 exit /b 1
call :build darwin amd64
exit /b %ERRORLEVEL%

:build
set "GOOS=%~1"
set "GOARCH=%~2"
set "EXT="
if /I "%~1"=="windows" set "EXT=.exe"
set "DEST=%OUT%\%~1-%~2\%NAME%%EXT%"
if not exist "%OUT%\%~1-%~2" mkdir "%OUT%\%~1-%~2"
echo -^> %DEST%
set CGO_ENABLED=0
go build -trimpath -ldflags="-s -w" -o "%DEST%" .
exit /b %ERRORLEVEL%

:help
echo usage: %~nx0 [native^|linux^|windows^|macos^|all]
echo   native   host GOOS/GOARCH (default)
echo   linux    linux/amd64 and linux/arm64
echo   windows  windows/amd64 and windows/arm64
echo   macos    darwin/arm64 and darwin/amd64
echo   all      linux, windows, and macos (amd64 + arm64)
echo OUT=dir overrides the output root (default: dist\)
exit /b 0
