#!/usr/bin/env bash
# wfc-server stack control: run.sh start|stop|status|restart|dbinit|logintest
# Loopback only. Uses project-local Go and Postgres (.local/). Logs: stack/logs/, pids: stack/logs/*.pid
set -u
HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
LOCAL="$ROOT/.local"
export PATH="$LOCAL/go/bin:$PATH" GOPATH="$LOCAL/gopath"
PG="$LOCAL/pg/bin"; PGDATA="$LOCAL/pgdata"; PGPORT=54329
# deployed server: no project-local Postgres, use the system one (Debian keeps it off PATH)
if [ ! -x "$PG/pg_ctl" ]; then
  PG="$(dirname "$(command -v pg_ctl || ls /usr/lib/postgresql/*/bin/pg_ctl 2>/dev/null | tail -1)")"
fi
LOGS="$HERE/logs"; mkdir -p "$LOGS"
WFC_PID="$LOGS/wfc.pid"

pg_up()  { "$PG/pg_ctl" -D "$PGDATA" status >/dev/null 2>&1; }
wfc_up() { [ -f "$WFC_PID" ] && kill -0 "$(cat "$WFC_PID")" 2>/dev/null; }

build() { (cd "$ROOT/wfc-server" && go build -o "$HERE/wfc-server-bin" .) ; }

start() {
  [ -d "$PGDATA" ] || "$PG/initdb" -D "$PGDATA" -U wfc -A trust >/dev/null || exit 1
  mkdir -p "$LOCAL/sock" && chmod 700 "$LOCAL/sock"   # private unix socket dir (trust auth is loopback/owner-only)
  if ! pg_up; then
    "$PG/pg_ctl" -D "$PGDATA" -l "$LOCAL/pg.log" -o "-p $PGPORT -k $LOCAL/sock" -w start || exit 1
  fi
  (cd "$HERE" && go run ./dbinit) || exit 1
  [ -x "$HERE/wfc-server-bin" ] || build || exit 1
  if wfc_up; then echo "wfc-server already running (pid $(cat "$WFC_PID"))"; return; fi
  ln -sf ../../wfc-server/game_list.tsv "$HERE/game_list.tsv"   # server reads it from cwd
  # per-start random admin API secret: the gateway forwards port 80 to this server, so a fixed secret would let players call /api/ban
  sed -i.bak "s|<apiSecret>[^<]*</apiSecret>|<apiSecret>$(openssl rand -hex 16)</apiSecret>|" "$HERE/config.xml" && rm -f "$HERE/config.xml.bak"
  # cwd must be stack/ : the server reads ./config.xml and writes ./state
  cd "$HERE" && { nohup ./wfc-server-bin >>"$LOGS/wfc.log" 2>&1 & echo $! >"$WFC_PID"; }
  for _ in $(seq 1 50); do
    curl -s -o /dev/null -H "Host: naswii.nintendowifi.net" "http://127.0.0.1:18080/nastest.jsp" && { echo "wfc-server up (pid $(cat "$WFC_PID"))"; return; }
    sleep 0.2
  done
  echo "wfc-server did not come up; see $LOGS/wfc.log"; exit 1
}

stop() {
  if wfc_up; then
    kill "$(cat "$WFC_PID")"   # frontend forwards shutdown to the backend child
    for _ in $(seq 1 25); do wfc_up || break; sleep 0.2; done
  fi
  rm -f "$WFC_PID"
  # backend child (integrated mode spawns "wfc-server-bin backend"); clean up strays
  pkill -f "wfc-server-bin" 2>/dev/null
  echo "wfc-server stopped (postgres left running; stop with: $PG/pg_ctl -D $PGDATA stop)"
}

status() {
  pg_up && echo "postgres: up (127.0.0.1:$PGPORT)" || echo "postgres: DOWN"
  wfc_up && echo "wfc-server: up (pid $(cat "$WFC_PID"))" || echo "wfc-server: DOWN"
  for p in tcp:18080 tcp:29900 tcp:29901 tcp:28910 tcp:29920 udp:27900 udp:27901; do
    proto=${p%%:*}; port=${p##*:}
    if lsof -nP -i"$proto:$port" 2>/dev/null | grep -q 127.0.0.1; then echo "  $p listening"; else echo "  $p NOT listening"; fi
  done
}

case "${1:-}" in
  start) start ;;
  stop) stop ;;
  restart) stop; start ;;
  status) status ;;
  build) build ;;
  dbinit) (cd "$HERE" && go run ./dbinit) ;;
  logintest) shift; (cd "$HERE" && go run ./logintest "$@") ;;
  *) echo "usage: $0 start|stop|restart|status|build|dbinit|logintest"; exit 2 ;;
esac
