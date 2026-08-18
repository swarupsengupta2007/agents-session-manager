#!/usr/bin/env sh
# Build agents-session-manager for the host, or cross-compile.
# Usage: ./build.sh [native|linux|windows|all]
set -eu
cd "$(dirname "$0")"

name="agents-session-manager"
outdir="${OUT:-dist}"
target="${1:-native}"

build_one() {
	goos="$1"
	goarch="$2"
	ext=""
	if [ "$goos" = "windows" ]; then
		ext=".exe"
	fi
	dest="${outdir}/${goos}-${goarch}/${name}${ext}"
	mkdir -p "$(dirname "$dest")"
	echo "→ ${dest}"
	CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" go build -trimpath -ldflags="-s -w" -o "$dest" .
}

case "$target" in
native)
	goos="$(go env GOOS)"
	goarch="$(go env GOARCH)"
	build_one "$goos" "$goarch"
	;;
linux)
	build_one linux amd64
	;;
windows)
	build_one windows amd64
	;;
all)
	build_one linux amd64
	build_one windows amd64
	;;
-h|--help|help)
	echo "usage: $0 [native|linux|windows|all]"
	echo "  native   host GOOS/GOARCH (default)"
	echo "  linux    linux/amd64"
	echo "  windows  windows/amd64"
	echo "  all      linux/amd64 and windows/amd64"
	echo "OUT=dir overrides the output root (default: dist/)"
	exit 0
	;;
*)
	echo "unknown target: $target (try native|linux|windows|all)" >&2
	exit 2
	;;
esac
