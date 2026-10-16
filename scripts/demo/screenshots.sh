#!/bin/sh
# Regenerates the board screenshots in docs/images from made-up demo data
# (TestDemoScreenshots in demo_test.go): an SVG of each, and a PNG at twice the
# size when rsvg-convert (librsvg) is installed. Nothing is read from bd, herdr
# or GitHub.
set -eu
cd "$(dirname "$0")/../.."
mkdir -p docs/images
QM_DEMO_DIR=docs/images go test -run '^TestDemoScreenshots$' -count=1 . >/dev/null
for svg in docs/images/board.svg docs/images/board-detail.svg; do
  if command -v rsvg-convert >/dev/null 2>&1; then
    rsvg-convert --zoom 2 --output "${svg%.svg}.png" "$svg"
  else
    echo "screenshots: rsvg-convert not found; wrote $svg only" >&2
  fi
done
