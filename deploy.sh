#!/usr/bin/env bash
# Full-stack deploy for the private SimpliConnection server.
#   build machine:  ./deploy.sh pack [out.tar]   bundle dist/ (game files!) + gateway + wfc-server sources
#   server:         ./deploy.sh install          build gateway + wfc-server (needs: go, postgresql, openssl, curl)
#                   ./deploy.sh up [invite-label]  start Postgres, wfc-server, gateway; prints the invite code
#                   ./deploy.sh status | down
# Env on `up`: PORT=8080  CERT=/path/cert.pem KEY=/path/key.pem (HTTPS; needed for non-localhost players)
# Postgres uses trust auth on loopback + a 0700 socket dir: run this on a dedicated/single-user box.
# dist/ holds Nintendo game data: keep the bundle private, never push it to a public host.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")" && pwd)"
GW="$ROOT/netgw/gw"; STACK="$ROOT/netgw/stack"; PORT="${PORT:-8080}"
GWPID="$GW/gw.pid"

gw_up() { [ -f "$GWPID" ] && kill -0 "$(cat "$GWPID")" 2>/dev/null; }

case "${1:-}" in
pack)
  OUT="${2:-$ROOT/../kart-deploy.tar}"
  [ -f "$ROOT/web/dist/index.html" ] || { echo "no web/dist: build first (web/build.sh)"; exit 1; }
  [ -d "$ROOT/wfc-server" ] || { echo "no wfc-server/ (patched WiiLink source) in $ROOT"; exit 1; }
  cd "$ROOT"
  # -h: follow dist/disc symlinks so the bundle holds the real files
  tar -chf "$OUT" --exclude='.git' --exclude='netgw-bin' --exclude='wfc-server-bin' --exclude='*.pid' \
    --exclude='netgw/gw/state' --exclude='netgw/stack/state' --exclude='netgw/stack/logs' --exclude='*.log' \
    --exclude='game_list.tsv.bak' --exclude='__pycache__' \
    web/dist netgw wfc-server deploy.sh
  ls -lh "$OUT" ;;
install)
  for c in go pg_ctl initdb openssl curl; do
    command -v "$c" >/dev/null || ls /usr/lib/postgresql/*/bin/"$c" >/dev/null 2>&1 || { echo "missing: $c"; exit 1; }
  done
  (cd "$GW" && go build -o netgw-bin .)
  bash "$STACK/run.sh" build
  echo "installed" ;;
up)
  [ -x "$GW/netgw-bin" ] && [ -x "$STACK/wfc-server-bin" ] || "$0" install
  bash "$STACK/run.sh" start
  if ! gw_up; then
    TLS=(); [ -n "${CERT:-}" ] && TLS=(--cert "$CERT" --key "$KEY" --https ":${HTTPS_PORT:-8443}")
    (cd "$GW" && exec nohup ./netgw-bin --dir "$ROOT/web/dist" --http ":$PORT" --state ./state ${TLS[@]+"${TLS[@]}"} >>gw.log 2>&1 </dev/null) &
    echo $! >"$GWPID"
    sleep 1; gw_up || { echo "gateway failed; see $GW/gw.log"; exit 1; }
  fi
  LABEL="${2:-friends}"
  (cd "$GW" && ./netgw-bin invite --state ./state list | grep -q "$LABEL" || ./netgw-bin invite --state ./state add "$LABEL")
  (cd "$GW" && ./netgw-bin invite --state ./state list)
  if [ -n "${CERT:-}" ]; then echo "open https://<this-host>:${HTTPS_PORT:-8443}/?name=You&code=<invite>  (plain http :$PORT also serves the page: firewall it)"
  else echo "open http://<this-host>:$PORT/?name=You&code=<invite>  (no TLS: LAN/VPN only; the invite code travels in clear)"; fi
  echo "NOTE: the page and game files (/disc/) are served without login; only online play needs the invite. Keep the host on a LAN/VPN or behind an authenticating proxy." ;;
down)
  gw_up && kill "$(cat "$GWPID")"; rm -f "$GWPID"
  bash "$STACK/run.sh" stop
  "$(dirname "$(command -v pg_ctl || ls /usr/lib/postgresql/*/bin/pg_ctl | tail -1)")/pg_ctl" -D "$ROOT/.local/pgdata" stop 2>/dev/null || true ;;
status)
  gw_up && echo "gateway: up (pid $(cat "$GWPID"))" || echo "gateway: DOWN"
  bash "$STACK/run.sh" status ;;
*) echo "usage: $0 pack [out.tar] | install | up [label] | down | status"; exit 2 ;;
esac
