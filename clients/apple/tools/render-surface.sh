#!/usr/bin/env bash
# Render an A2UI messages JSON with the production renderer, style and container.
# Usage: render-surface.sh INPUT.json OUTPUT_PREFIX [WIDTHxHEIGHT]
# Writes OUTPUT_PREFIX-light.png and OUTPUT_PREFIX-dark.png (and -errors.txt on render errors).
# Facts and limits: docs/development/verification.md#offline-surface-rendering
set -eu

[ "$#" -ge 2 ] || { sed -n '2,5p' "$0" >&2; exit 2; }
input=$(cd "$(dirname "$1")" && pwd)/$(basename "$1")
[ -f "$input" ] || { echo "ERROR: input not found: $1" >&2; exit 2; }
prefix=$2
case "$prefix" in /*) ;; *) prefix=$PWD/$prefix ;; esac
size=${3:-400x480}

package=$(cd "$(dirname "$0")/../Packages/ZoeKit" && pwd)
cd "$package"
ZOE_RENDER_INPUT=$input ZOE_RENDER_OUTPUT=$prefix ZOE_RENDER_SIZE=$size \
    swift test --filter RenderHarnessTests
