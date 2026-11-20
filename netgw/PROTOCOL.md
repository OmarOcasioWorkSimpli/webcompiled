# Browser <-> gateway wire protocol (v1)

One WebSocket per browser tab: `GET /net?v=1&dev=<deviceId>&name=<displayName>&code=<inviteCode>`.
- `dev`: 16 lowercase hex chars, generated once per browser (IndexedDB/localStorage). It is the dummy account identity.
- `code`: invite code. Gateway rejects the upgrade with HTTP 403 if unknown/revoked; 409 if `dev` is already connected.
- First server message is a TEXT frame, JSON: `{"vip":"10.77.A.B","gw":"10.77.0.1","name":"...","userId":"<decimal u64>","serno":"<9 digits>","mac":"00:09:bf:xx:xx:xx"}`.
  All identity values are derived deterministically from `dev` by the gateway (HMAC with a server secret), so reconnects keep the same identity.
- Everything after that is BINARY frames, little-endian: `u8 type | u32 id | payload`. `id` is a client-chosen socket/request id.
- Max frame 2048 bytes payload for UDP; TCP chunks up to 16 KiB. No compression (no permessage-deflate). TCP_NODELAY on the server side.

| type | dir | name | payload |
|---|---|---|---|
| 0x10 | c->s | DNS_REQ | utf8 hostname (id = request id) |
| 0x11 | s->c | DNS_RESP | `u8 ok` `u32 ipv4 (network byte order)` |
| 0x20 | c->s | TCP_OPEN | `u32 dstip (NBO)` `u16 dstport` |
| 0x21 | s->c | TCP_OPENED | `u8 status` (0 ok, else errno: 111 refused, 113 unreachable, 110 timeout) |
| 0x22 | both | TCP_DATA | bytes |
| 0x23 | both | TCP_CLOSE | (none) |
| 0x30 | c->s | UDP_BIND | `u16 port` (0 = pick) |
| 0x31 | s->c | UDP_BOUND | `u8 status` `u16 port` |
| 0x32 | c->s | UDP_SEND | `u32 dstip (NBO)` `u16 dstport` bytes |
| 0x33 | s->c | UDP_RECV | `u32 srcip (NBO)` `u16 srcport` bytes |
| 0x34 | c->s | UDP_CLOSE | (none) |
| 0x40 | both | PING | echo, for latency (id = nonce) |

## Addressing (virtual network)
- Gateway service address `gw` = 10.77.0.1. Every `*.nintendowifi.net` and `*.rwfc.net` name resolves to `gw` (DNS_RESP). Real DNS is never used.
- Client virtual IP `vip` = 10.77.A.B, stable per `dev`. The runtime returns it from SO GETHOSTID / GETINTERFACEOPT.
- TCP to `gw:port`: the gateway dials `127.0.0.1:<mapped port>` where wfc-server listens (map file `netgw/services.json`: `{ "80": 8080, "443": 8080, ... }`; HTTPS ports are served as plain HTTP because the runtime reroutes 443->80 plaintext).
- UDP to `gw:27900` (QR2) / `gw:27901` (NATNEG): the gateway owns ONE real UDP socket per (client, bound port) bound on 127.0.0.1:<ephemeral>, and forwards to wfc-server at 127.0.0.1:27900 / 27901. wfc-server therefore sees each client as `127.0.0.1:<ephemeral>` (a distinct ip:port per client socket). Replies from wfc-server arrive on that real socket and go back as UDP_RECV with src = `gw:27900/27901`.
- Peer UDP (NATNEG hands a peer an address that is either `127.0.0.1:<someone's ephemeral port>` (public) or `10.77.A.B:<port>` (local ip the game reported)). `UDP_SEND` to either form is resolved by the gateway to the owning client socket and delivered to that client as UDP_RECV with `srcip` = sender's `vip` and `srcport` = sender's bound port. Unknown destinations are dropped silently (UDP semantics). The gateway never parses game payloads.
- Receive ordering is per-socket FIFO. Queue per client is bounded (drop oldest UDP beyond ~512 datagrams).
- Rooms are not a gateway concept: the 12-player cap and room allocation are enforced by the game (DWC maxEntry=12) and wfc-server groups.

## Server -> client text frames after hello
`{"evt":"kick","reason":"..."}` then close. `{"evt":"info","players":N}` optional.
