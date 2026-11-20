# Private online play (home LAN)

Browser wasm -> one WebSocket (`/net`) -> `netgw` (invite check, virtual NAT, UDP relay) -> local WiiLink `wfc-server` + PostgreSQL.
Wire spec: `PROTOCOL.md`. No Nintendo services, no domain.

## Start
```
cd webcompiled
.local/pg/bin/pg_ctl -D .local/pgdata -l .local/pg.log start   # Postgres
bash netgw/stack/run.sh start                                  # wfc-server (loopback only)
cd netgw/gw && ./netgw-bin --dir ../../web/dist --http 0.0.0.0:8080 --state ./state
```
WebGPU needs a secure context: open `http://localhost:8080` on the server box. Other LAN devices need HTTPS
(see `docs/web-hosting.md`; `serve.py --https --lan` style cert, then put the gateway behind it).

## Invites / accounts
```
./netgw-bin invite --state ./state add friends     # prints a code like ABCD-EFGH
./netgw-bin invite --state ./state list | revoke <label|code>
```
Players open `/?name=Alice&code=ABCD-EFGH` (or type them on the start screen). The browser keeps a random device id
(localStorage `mkw.dev`); the gateway derives the console identity (serno, MAC, 16-digit friend code) from it. No passwords.

## Rooms
The game caps a room at 12 (DWC maxEntry). A 13th searcher finds no open room and the game opens a new one on the same
server, so there is no separate "second server" to run.

## Debug
`curl 127.0.0.1:8080/status` (players, UDP sockets). `NETGW_TRACE=1` logs every client frame.
Load test: `go run ./cmd/loadgen -url ws://127.0.0.1:8080/net -code <code> -n 12 -rate 60 -dur 10s`.

## Local patch
`wfc-server/gpcm/login.go` + `common/config.go`: `allowUnauthenticatedClients` (config.xml) lets a stock, unpatched
Wii client log in and matchmake. Required: the browser client has no `wl:sig`.

## Game patches (run by `web/build.sh`)
`web/tools/patch_simpli_ui.py` renames Nintendo Wi-Fi strings to SimpliConnection in `Scene/UI/*.szs`.
`web/tools/patch_simpli_game.py` patches staged StaticR.rel (no idle drop, skip WFCMainMenu). Both idempotent.
