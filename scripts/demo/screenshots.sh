#!/bin/sh
# Regenerates the board screenshots and recording in docs/images from made-up
# demo data (TestDemoScreenshots and TestDemoRecording in demo_test.go): an SVG
# of each screen, a PNG at twice the size when rsvg-convert (librsvg) is
# installed, and board.gif from the recording's frames when ImageMagick is too.
# Nothing is read from bd, herdr or GitHub.
set -eu
cd "$(dirname "$0")/../.."
mkdir -p docs/images
frames=docs/images/board-frames
rm -rf "$frames"
QM_DEMO_DIR=docs/images go test -run '^TestDemo(Screenshots|Recording)$' -count=1 . >/dev/null
if ! command -v rsvg-convert >/dev/null 2>&1; then
  echo "screenshots: rsvg-convert not found; wrote the SVGs only" >&2
  exit 0
fi
for svg in docs/images/board.svg docs/images/board-detail.svg docs/images/board-tidy.svg; do
  rsvg-convert --zoom 2 --output "${svg%.svg}.png" "$svg"
done
if command -v magick >/dev/null 2>&1; then
  for svg in "$frames"/*.svg; do
    rsvg-convert --output "${svg%.svg}.png" "$svg"
  done
  # Each step holds a second; the first and last frames hold longer.
  set -- "$frames"/*.png
  first=$1
  shift $(($# - 1))
  last=$1
  magick -delay 300 "$first" -delay 90 "$frames"/*.png -delay 300 "$last" -loop 0 -layers Optimize docs/images/board.gif
else
  echo "screenshots: ImageMagick not found; no board.gif" >&2
fi
rm -rf "$frames"
