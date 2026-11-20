// node web/tests/net_codec_test.mjs : codec + queue checks for web/site/net.js (no browser, no gateway).
import { createRequire } from 'node:module';
import assert from 'node:assert/strict';
const { T, encode, decode, Gateway, gatewayUrl } = createRequire(import.meta.url)('../site/net.js');

// codec: little-endian id, raw payload
const f = encode(T.UDP_SEND, 0x01020304, Uint8Array.of(10, 77, 0, 1, 0x6c, 0xda, 9));
assert.deepEqual([...f.subarray(0, 5)], [0x32, 4, 3, 2, 1]);
const d = decode(f);
assert.equal(d.type, T.UDP_SEND); assert.equal(d.id, 0x01020304); assert.equal(d.payload.length, 7);

// fake websocket
class WS {
  constructor() { this.readyState = 1; this.sent = []; WS.last = this; }
  send(b) { this.sent.push(decode(b)); }
}
const g = new Gateway(WS);
assert.equal(g.alloc('tcp'), 0, 'no sockets before hello');
const p = g.connect('ws://x/net');
WS.last.onmessage({ data: JSON.stringify({ vip: '10.77.1.2', gw: '10.77.0.1', userId: '5', serno: '000000001', mac: '00:09:bf:00:00:01' }) });
const hello = await p;
assert.equal(hello.vip, '10.77.1.2');
const push = (type, id, bytes) => WS.last.onmessage({ data: encode(type, id, Uint8Array.from(bytes)).buffer });

// DNS
const dns = g.alloc('dns');
g.send(T.DNS_REQ, dns, new TextEncoder().encode('naswii.nintendowifi.net'));
assert.equal(WS.last.sent[0].id, dns);
push(T.DNS_RESP, dns, [1, 10, 77, 0, 1]);
assert.equal(g.socks.get(dns).dns, 1); assert.equal(g.socks.get(dns).ip, 0x0a4d0001);

// TCP stream: two chunks, partial reads, peek
const tcp = g.alloc('tcp');
push(T.TCP_OPENED, tcp, [0]); assert.equal(g.socks.get(tcp).opened, 0);
push(T.TCP_DATA, tcp, [1, 2, 3]); push(T.TCP_DATA, tcp, [4, 5]);
assert.equal(g.rxLen(tcp), 5);
let buf = new Uint8Array(4);
assert.equal(g.take(tcp, buf, 4, true).n, 4); assert.equal(g.rxLen(tcp), 5, 'peek keeps data');
assert.equal(g.take(tcp, buf, 4, false).n, 4); assert.deepEqual([...buf], [1, 2, 3, 4]);
assert.equal(g.take(tcp, buf, 4, false).n, 1); assert.equal(g.rxLen(tcp), -1);
push(T.TCP_CLOSE, tcp, []); assert.ok(g.socks.get(tcp).closed);

// UDP: datagram boundaries, src addr, truncation
const udp = g.alloc('udp');
push(T.UDP_RECV, udp, [10, 77, 0, 1, 0x6c, 0x6d, 7, 8, 9]);
push(T.UDP_RECV, udp, [127, 0, 0, 1, 1, 0, 6]);
assert.equal(g.rxLen(udp), 3);
buf = new Uint8Array(2);
const r = g.take(udp, buf, 2, false);
assert.deepEqual([r.n, r.ip, r.port], [2, 0x0a4d0001, 0x6d6c]);
assert.equal(g.take(udp, buf, 2, false).port, 1);
assert.equal(g.take(udp, buf, 2, false), null);

// frames for freed sockets are dropped; closing the socket marks everything closed
g.free(udp); push(T.UDP_RECV, udp, [1, 2, 3, 4, 0, 0, 1]);
WS.last.onclose(); assert.ok(g.socks.get(tcp).closed); assert.equal(g.open, false);

assert.equal(gatewayUrl({ protocol: 'https:', host: 'h:8443' }, 'ab', 'Bob Z', 'c'), 'wss://h:8443/net?v=1&dev=ab&name=Bob+Z&code=c');
console.log('PASS net codec');
