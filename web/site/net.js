// Gateway client for private online play (wire spec: netgw/PROTOCOL.md, v1).
//
// Owns the one WebSocket and the per-socket receive queues. The wasm runtime reaches it only
// through the plain synchronous imports in web/js/library_mkw_net.js (Module.mkwNet), polling
// the queues from its scheduler; nothing here ever calls into the guest.
// Frames: u8 type | u32 id | payload, little-endian (ports too); IPv4 addresses are raw
// network-order bytes. Also loads under node (tests/net_codec_test.mjs).
(function (root) {
  'use strict';

  const T = {
    DNS_REQ: 0x10, DNS_RESP: 0x11, TCP_OPEN: 0x20, TCP_OPENED: 0x21, TCP_DATA: 0x22,
    TCP_CLOSE: 0x23, UDP_BIND: 0x30, UDP_BOUND: 0x31, UDP_SEND: 0x32, UDP_RECV: 0x33,
    UDP_CLOSE: 0x34, PING: 0x40,
  };
  const MAX_UDP_QUEUE = 512; // same bound the gateway applies

  function encode(type, id, payload) {
    const n = payload ? payload.length : 0;
    const out = new Uint8Array(5 + n);
    out[0] = type;
    new DataView(out.buffer).setUint32(1, id >>> 0, true);
    if (n) out.set(payload, 5);
    return out;
  }

  function decode(bytes) {
    if (bytes.length < 5) return null;
    const id = new DataView(bytes.buffer, bytes.byteOffset, bytes.length).getUint32(1, true);
    return { type: bytes[0], id, payload: bytes.subarray(5) };
  }

  /** 4 network-order bytes -> host-order u32. */
  function ipFromBytes(b, o) { return ((b[o] << 24) | (b[o + 1] << 16) | (b[o + 2] << 8) | b[o + 3]) >>> 0; }

  class Gateway {
    constructor(WebSocketImpl) {
      this.WS = WebSocketImpl || root.WebSocket;
      this.ws = null;
      this.hello = null;   // parsed hello JSON once received
      this.open = false;   // hello received and socket still up
      this.kicked = '';
      this.socks = new Map();
      this.nextId = 1;
    }

    /** Resolves with the hello object; rejects if the upgrade is refused or the socket drops first. */
    connect(url) {
      return new Promise((resolve, reject) => {
        let settled = false;
        const fail = (why) => { if (!settled) { settled = true; reject(new Error(why)); } };
        let ws;
        try { ws = new this.WS(url); } catch (e) { fail(String(e && e.message || e)); return; }
        this.ws = ws;
        ws.binaryType = 'arraybuffer';
        ws.onmessage = (ev) => {
          if (typeof ev.data === 'string') {
            let msg;
            try { msg = JSON.parse(ev.data); } catch (_) { return; }
            if (!this.hello) {
              this.hello = msg;
              this.open = true;
              settled = true;
              resolve(msg);
            } else if (msg.evt === 'kick') {
              this.kicked = String(msg.reason || 'kicked');
            }
            return;
          }
          if (this.hello) this._frame(decode(new Uint8Array(ev.data)));
        };
        const dropped = () => {
          this.open = false;
          for (const s of this.socks.values()) s.closed = true;
          // Browsers hide the HTTP status of a refused upgrade, so the page can only say "refused".
          fail('refused');
        };
        ws.onerror = dropped;
        ws.onclose = dropped;
      });
    }

    close() { try { this.ws && this.ws.close(); } catch (_) { /* already gone */ } }

    alloc(kind) {
      if (!this.open) return 0;
      const id = this.nextId++;
      this.socks.set(id, { kind: kind || 'tcp', rx: [], rxBytes: 0, opened: -1, closed: false, bound: -1, dns: -1, ip: 0 });
      return id;
    }

    free(id) { this.socks.delete(id); }

    send(type, id, payload) {
      if (!this.open || this.ws.readyState !== 1) return false;
      this.ws.send(encode(type, id, payload));
      return true;
    }

    /** Bytes (TCP) or size of the next datagram (UDP) waiting, -1 if none. */
    rxLen(id) {
      const s = this.socks.get(id);
      if (!s || !s.rx.length) return -1;
      return s.kind === 'udp' ? s.rx[0].data.length : s.rxBytes;
    }

    /**
     * Copy up to cap bytes into dst. TCP is a stream (consumed unless peek); UDP returns one
     * datagram truncated to cap. Returns {n, ip, port} or null when nothing is queued.
     */
    take(id, dst, cap, peek) {
      const s = this.socks.get(id);
      if (!s || !s.rx.length) return null;
      if (s.kind === 'udp') {
        const d = peek ? s.rx[0] : s.rx.shift();
        const n = Math.min(cap, d.data.length);
        dst.set(d.data.subarray(0, n));
        return { n, ip: d.ip, port: d.port };
      }
      let n = 0;
      let i = 0;
      while (n < cap && i < s.rx.length) {
        const chunk = s.rx[i];
        const take = Math.min(cap - n, chunk.length);
        dst.set(chunk.subarray(0, take), n);
        n += take;
        if (take < chunk.length) {
          if (!peek) s.rx[i] = chunk.subarray(take);
          break;
        }
        i++;
      }
      if (!peek) { s.rx.splice(0, i); s.rxBytes -= n; }
      return { n, ip: 0, port: 0 };
    }

    _frame(f) {
      if (!f) return;
      if (f.type === T.PING) { this.send(T.PING, f.id, f.payload); return; }
      const s = this.socks.get(f.id);
      if (!s) return; // freed locally: drop (late data for a closed socket)
      const p = f.payload;
      switch (f.type) {
        case T.DNS_RESP:
          if (p.length >= 5) { s.dns = p[0] ? 1 : 0; s.ip = ipFromBytes(p, 1); }
          break;
        case T.TCP_OPENED: s.opened = p.length ? p[0] : 111; break;
        case T.TCP_DATA: {
          const c = p.slice(); // detach from the frame buffer
          s.rx.push(c); s.rxBytes += c.length;
          break;
        }
        case T.TCP_CLOSE: s.closed = true; break;
        case T.UDP_BOUND: if (p.length >= 3) s.bound = p[0]; break;
        case T.UDP_RECV:
          if (p.length >= 6) {
            s.rx.push({ ip: ipFromBytes(p, 0), port: p[4] | (p[5] << 8), data: p.slice(6) });
            if (s.rx.length > MAX_UDP_QUEUE) s.rx.shift();
          }
          break;
      }
    }
  }

  /** wss?://<host>/net?v=1&dev=..&name=..&code=.. for the page's own origin. */
  function gatewayUrl(loc, dev, name, code) {
    const q = new URLSearchParams({ v: '1', dev, name, code });
    return (loc.protocol === 'https:' ? 'wss://' : 'ws://') + loc.host + '/net?' + q.toString();
  }

  const api = { T, encode, decode, ipFromBytes, Gateway, gatewayUrl };
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
  else root.MkwNet = api;
})(typeof globalThis !== 'undefined' ? globalThis : this);
