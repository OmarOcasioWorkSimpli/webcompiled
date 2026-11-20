// Gateway socket imports for the web runtime (runtime/src/platform/web/web_net.cpp).
// Everything is a plain synchronous call into Module.mkwNet (web/site/net.js), which owns the
// WebSocket and the receive queues. Nothing here suspends (a JSPI-suspending import would freeze
// the whole console) and nothing calls back into wasm: the runtime polls from its scheduler.
// Offline (no gateway configured) Module.mkwNet is absent and every import reports "no network".

addToLibrary({
  mkw_net_open: () => { const n = Module['mkwNet']; return n && n.open ? 1 : 0; },

  // kind: 0 tcp, 1 udp, 2 dns request. Returns the socket / request id, 0 when offline.
  mkw_net_alloc: (kind) => {
    const n = Module['mkwNet'];
    return n ? n.alloc(kind == 1 ? 'udp' : kind == 2 ? 'dns' : 'tcp') : 0;
  },

  mkw_net_free: (id) => { const n = Module['mkwNet']; if (n) n.free(id); },

  mkw_net_send: (type, id, ptr, len) => {
    const n = Module['mkwNet'];
    return n && n.send(type, id, len ? HEAPU8.slice(ptr, ptr + len) : null) ? 1 : 0;
  },

  // what: 0 TCP_OPENED status (-1 pending), 1 closed by peer/gateway, 2 UDP_BOUND status (-1 pending),
  //       3 DNS state (-1 pending, 0 failed, 1 ok), 4 DNS ip (host order), 5 next rx length (-1 none).
  mkw_net_status: (id, what) => {
    const n = Module['mkwNet'];
    const s = n && n.socks.get(id);
    if (!s) return what == 1 ? 1 : -1;
    switch (what) {
      case 0: return s.opened;
      case 1: return s.closed ? 1 : 0;
      case 2: return s.bound;
      case 3: return s.dns;
      case 4: return s.ip | 0;
      default: return n.rxLen(id);
    }
  },

  // Copies queued data into dst. UDP also writes [srcip, srcport] (host order u32 x2) to info.
  // Returns bytes copied, or -1 when nothing is queued. flags bit 0: peek.
  mkw_net_recv: (id, dst, cap, flags, info) => {
    const n = Module['mkwNet'];
    if (!n) return -1;
    const r = n.take(id, HEAPU8.subarray(dst, dst + cap), cap, flags & 1);
    if (!r) return -1;
    if (info) { HEAPU32[info >> 2] = r.ip; HEAPU32[(info >> 2) + 1] = r.port; }
    return r.n;
  },

  // idx: 0 vip, 1 gw, 2 userId, 3 serno, 4 mac, 5 name. Writes a NUL-terminated string; returns its length (0 = none).
  mkw_net_hello__deps: ['$stringToUTF8', '$lengthBytesUTF8'],
  mkw_net_hello: (idx, ptr, cap) => {
    const n = Module['mkwNet'];
    const h = n && n.hello;
    const v = h && h[['vip', 'gw', 'userId', 'serno', 'mac', 'name'][idx]];
    if (v === undefined || v === null || cap < 1) return 0;
    const s = String(v);
    stringToUTF8(s, ptr, cap);
    return Math.min(lengthBytesUTF8(s), cap - 1);
  },
});
