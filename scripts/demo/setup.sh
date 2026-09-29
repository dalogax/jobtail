#!/usr/bin/env bash
# Builds jobtail and seeds a throwaway data dir with a few jobs and some run
# history, for recording the README demo (see demo.tape). Touches nothing
# outside $DEMO_DIR. Prints the env to source afterwards.
set -euo pipefail

repo="$(cd "$(dirname "$0")/../.." && pwd)"
demo="${DEMO_DIR:-/tmp/jobtail-demo}"

rm -rf "$demo"
mkdir -p "$demo/bin" "$demo/home/scripts" "$demo/home/code/shop"
go build -C "$repo" -o "$demo/bin/jobtail" ./cmd/jobtail
cp "$repo/scripts/demo/fake_claude.sh" "$demo/bin/claude"
# Shadows any real herdr on PATH: see fake_herdr.sh for why the demo must not
# reach one.
cp "$repo/scripts/demo/fake_herdr.sh" "$demo/bin/herdr"

cat > "$demo/home/scripts/check_backups.sh" <<'SH'
#!/bin/sh
echo "checking snapshots on mars..."
echo "tank/photos   last snapshot 14m ago   ok"
echo "tank/docs     last snapshot 14m ago   ok"
echo "all backups fresh"
SH
cat > "$demo/home/scripts/disk_space.sh" <<'SH'
#!/bin/sh
echo "/      41% used"
echo "/home  63% used"
SH
cat > "$demo/home/scripts/check_certs.sh" <<'SH'
#!/bin/sh
echo "api.example.com     expires in 61 days   ok"
echo "shop.example.com    expires in 3 days    EXPIRING" >&2
echo "1 certificate needs renewal" >&2
exit 1
SH
chmod +x "$demo"/home/scripts/*.sh

export JOBTAIL_DATA_DIR="$demo/data" JOBTAIL_DISABLE_NOTIFY=1 \
  JOBTAIL_CLAUDE_BIN="$demo/bin/claude"
jt="$demo/bin/jobtail"
s="$demo/home/scripts"

$jt add disk-space  --kind cli --cron "0 * * * *"    --cwd "$s" --cmd "./disk_space.sh"  >/dev/null
$jt add cert-expiry --kind cli --cron "0 8 * * *"    --cwd "$s" --cmd "./check_certs.sh" >/dev/null
for _ in 1 2 3; do $jt run disk-space  >/dev/null 2>&1 || true; done
for _ in 1 2;   do $jt run cert-expiry >/dev/null 2>&1 || true; done

echo "export HOME='$demo/home' PATH='$demo/bin':\$PATH JOBTAIL_DATA_DIR='$demo/data' JOBTAIL_DISABLE_NOTIFY=1 JOBTAIL_CLAUDE_BIN='$demo/bin/claude'"
