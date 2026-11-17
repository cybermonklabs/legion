#!/bin/sh
# Writes InterVariable-subset.woff2 beside this file: the typeface the Dispatch web app ships and
# draws the issue page in (`web/src/styles.css`, dispatch://INFRA-1196 R36), cut from rsms/inter's
# own release. It starts from the release's `web/InterVariable.woff2`, the file the approved
# prototype of the issue page embeds, checked by the SHA-256 of the release archive and of that
# file, and keeps:
#
# - the weight axis from 400 to 700, the weights the app draws, and the optical size axis whole
#   (14 to 32), which a browser sets from the font size as it did when the prototype was drawn;
# - the characters of UNICODES below, which `unicode-range` in styles.css repeats so a browser
#   takes any other character from the next family of the stack without fetching this file:
#   Basic Latin and Latin-1, the rest of Google Fonts' `latin` subset, General Punctuation (the
#   dashes, quotes, bullet, middle dot and ellipsis), the Arrows block, and the marks the app and
#   the prototype draw that Inter has (≈ ⊘ ⌘ ⌥ ▲ ▼ ◆ ○ ● ⚠ ✓ ✗);
# - fonttools' default OpenType features plus `tnum`, which Tailwind's `tabular-nums` asks for,
#   and the licence's name records (IDs 13 and 14) beside the default ones.
#
# The release's LICENSE.txt is this directory's LICENSE.txt, which the script compares, and
# package.json is what `scripts/third-party-notices.ts` reads to list the typeface in the build's
# THIRD_PARTY_NOTICES.txt. fonttools runs through uvx, pinned (skills/legion-worker/references/
# tools.md); the instancer keeps the source's timestamp, so the same inputs write the same bytes.
#
# The cut is held to SUBSET_SHA256, the SHA-256 of the file committed beside this script: the
# script writes it to its scratch directory, checks it, and moves it into place only when it is
# those bytes, so a fonttools, brotli or default that moved refuses rather than ships a font no
# one read. A change made on purpose - to UNICODES, a pin or the release - records the new
# file's SHA-256, which the script prints before it checks, in SUBSET_SHA256.
#
#   packages/dispatch/web/src/fonts/inter/subset.sh
set -eu

RELEASE_URL=https://github.com/rsms/inter/releases/download/v4.1/Inter-4.1.zip
RELEASE_SHA256=9883fdd4a49d4fb66bd8177ba6625ef9a64aa45899767dde3d36aa425756b11e
SOURCE=web/InterVariable.woff2
SOURCE_SHA256=693b77d4f32ee9b8bfc995589b5fad5e99adf2832738661f5402f9978429a8e3
SUBSET_SHA256=f0fd4bf76ee26cd57f6dea4145816a67f7e2f7c1917d3325ffc98279c95235d9
UNICODES=U+0000-00FF,U+0131,U+0152-0153,U+02BB-02BC,U+02C6,U+02DA,U+02DC,U+0304,U+0308,U+0329,U+2000-206F,U+20AC,U+2122,U+2190-21FF,U+2212,U+2215,U+2248,U+2298,U+2318,U+2325,U+25B2,U+25BC,U+25C6,U+25CB,U+25CF,U+26A0,U+2713,U+2717,U+FEFF,U+FFFD

here="$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)"
scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT

curl -fsSL -o "$scratch/inter.zip" "$RELEASE_URL"
echo "$RELEASE_SHA256  $scratch/inter.zip" | sha256sum -c -
unzip -q -d "$scratch" "$scratch/inter.zip" "$SOURCE" LICENSE.txt
echo "$SOURCE_SHA256  $scratch/$SOURCE" | sha256sum -c -
cmp "$scratch/LICENSE.txt" "$here/LICENSE.txt"

uvx --from fonttools==4.68.0 --with brotli==1.2.1 fonttools varLib.instancer -q \
  --no-recalc-timestamp "$scratch/$SOURCE" wght=400:700 -o "$scratch/weights.woff2"
uvx --from fonttools==4.68.0 --with brotli==1.2.1 pyftsubset "$scratch/weights.woff2" \
  --unicodes="$UNICODES" --layout-features+=tnum --name-IDs+=13,14 --flavor=woff2 \
  --output-file="$scratch/subset.woff2"
sha256sum "$scratch/subset.woff2"
echo "$SUBSET_SHA256  $scratch/subset.woff2" | sha256sum -c -
mv "$scratch/subset.woff2" "$here/InterVariable-subset.woff2"
wc -c "$scratch/$SOURCE" "$here/InterVariable-subset.woff2"
