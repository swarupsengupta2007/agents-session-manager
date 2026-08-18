#!/usr/bin/env sh
# Build agents-session-manager for macOS (Apple Silicon and/or Intel).
# Can be run on Linux, macOS, or Windows (Git Bash) — it cross-compiles.
# Usage: ./build-macos.sh [arm64|amd64|all]
set -eu
cd "$(dirname "$0")"

name="agents-session-manager"
outdir="${OUT:-dist}"
target="${1:-all}"

build_one() {
	goarch="$1"
	dest="${outdir}/darwin-${goarch}/${name}"
	mkdir -p "$(dirname "$dest")"
	echo "→ ${dest}"
	CGO_ENABLED=0 GOOS=darwin GOARCH="$goarch" go build -trimpath -ldflags="-s -w" -o "$dest" .
}

case "$target" in
arm64|aarch64|apple|m1|silicon)
	build_one arm64
	;;
amd64|x86_64|x64|intel)
	build_one amd64
	;;
all|macos|darwin|mac|"")
	build_one arm64
	build_one amd64
	;;
native)
	# On a Mac this matches the host CPU; elsewhere it still builds darwin.
	arch="$(uname -m 2>/dev/null || true)"
	case "$arch" in
	arm64|aarch64) build_one arm64 ;;
	*) build_one amd64 ;;
	esac
	;;
-h|--help|help)
	echo "usage: $0 [arm64|amd64|all|native]"
	echo "  all      darwin/arm64 and darwin/amd64 (default)"
	echo "  arm64    Apple Silicon (M1/M2/M3/…)"
	echo "  amd64    Intel Mac"
	echo "  native   this machine's CPU (Apple Silicon vs Intel)"
	echo "OUT=dir overrides the output root (default: dist/)"
	exit 0
	;;
*)
	echo "unknown target: $target (try arm64|amd64|all|native)" >&2
	exit 2
	;;
esac
