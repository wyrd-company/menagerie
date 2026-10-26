#!/usr/bin/env bash
# Records the README's demo, docs/user/assets/demo.gif, and the documentation's
# screenshots in docs/user/assets, against a fresh acceptance fixture.
#
#   docs/demo/record.sh <scratch>
#
# <scratch> holds a fixture that web/test/integrated/acceptance.sh up and
# fixture started, before any other browser run used it; `task demo` does
# all of it. Needs ffmpeg, gifsicle and ImageMagick.
set -euo pipefail
scratch=$(realpath -m "$1")
repo=$(git -C "$(dirname "$0")" rev-parse --show-toplevel)
out=$scratch/demo
rm -rf "$out"
mkdir -p "$out" "$repo/docs/user/assets"
MENAGERIE_BROWSER_INFO="$scratch/info.json" MENAGERIE_DEMO_ASSETS="$repo/docs/user/assets" MENAGERIE_DEMO_OUTPUT="$out" \
  pnpm --dir "$repo/web" exec playwright test -c playwright.demo.config.ts
video=$(find "$out" -name '*.webm' | head -n 1)
[ -n "$video" ] || { echo "the demo run recorded no video" >&2; exit 1; }
# Five frames a second at 720 pixels wide keeps the GIF near 2 MB.
ffmpeg -loglevel error -y -i "$video" \
  -vf "fps=5,scale=720:-1:flags=lanczos,split[a][b];[a]palettegen=max_colors=64:stats_mode=diff[p];[b][p]paletteuse=dither=bayer:bayer_scale=5:diff_mode=rectangle" \
  "$out/demo.gif"
gifsicle -O3 --lossy=100 "$out/demo.gif" -o "$repo/docs/user/assets/demo.gif"
for shot in list create-interview upgrade-interview; do
  magick "$repo/docs/user/assets/$shot.png" -strip -colors 256 "PNG8:$repo/docs/user/assets/$shot.png"
done
ls -l "$repo/docs/user/assets"
