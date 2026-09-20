#!/bin/sh
# Refresh internal/api/webui from a status/ checkout (default: ../status).
# Usage: STATUS_DIR=/path/to/status scripts/refresh-webui.sh
# Rebuilds the SPA, vendors index.html + hashed assets + embed.js +
# favicon.svg, and stamps the origin in source.txt. webui tests refuse
# literal hashes, so a refresh never breaks them by renaming files.
set -eu
STATUS_DIR="${STATUS_DIR:-../status}"
DEST="internal/api/webui"

test -f "$STATUS_DIR/package.json" || {
  echo "no status checkout at $STATUS_DIR (override with STATUS_DIR=...)" >&2
  exit 1
}
(cd "$STATUS_DIR" && npm run build >/dev/null) || exit 1
rm -rf "$DEST/index.html" "$DEST/favicon.svg" "$DEST/embed.js" "$DEST/assets"
cp "$STATUS_DIR/dist/index.html" "$STATUS_DIR/dist/favicon.svg" \
   "$STATUS_DIR/dist/embed.js" "$DEST/"
cp -R "$STATUS_DIR/dist/assets" "$DEST/assets"
rev="(unknown)"
if [ -d "$STATUS_DIR/.git" ]; then
  rev="$(git -C "$STATUS_DIR" rev-parse --short HEAD 2>/dev/null || echo unknown)"
  if [ -n "$(git -C "$STATUS_DIR" status --short 2>/dev/null)" ]; then
    rev="$rev-dirty"
  fi
fi
printf 'origin: epmon-dev/status @ %s\nrefreshed: %s\n' "$rev" "$(date -u +%FT%TZ)" > "$DEST/source.txt"
echo "webui refreshed from $STATUS_DIR ($rev)"
