package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestFramingAndQueue(t *testing.T) {
	f := frame(tUDPSend, 0x01020304, []byte{10, 77, 1, 2}, le16(27900), []byte("hi"))
	if f[0] != tUDPSend || binary.LittleEndian.Uint32(f[1:]) != 0x01020304 || [4]byte(f[5:9]) != [4]byte{10, 77, 1, 2} ||
		binary.LittleEndian.Uint16(f[9:]) != 27900 || string(f[11:]) != "hi" {
		t.Fatalf("bad frame % x", f)
	}
	q := newQueue()
	q.push([]byte("tcp"), false)
	for i := 0; i < maxQUDP+10; i++ {
		q.push([]byte{byte(i), byte(i >> 8)}, true)
	}
	if q.nudp != maxQUDP {
		t.Fatalf("nudp=%d", q.nudp)
	}
	b, _ := q.pop()
	if string(b) != "tcp" {
		t.Fatal("tcp frame must never be dropped")
	}
	b, _ = q.pop()
	if b[0] != 10 { // 10 oldest dropped
		t.Fatalf("oldest not dropped: %v", b)
	}
}

func TestInvites(t *testing.T) {
	p := filepath.Join(t.TempDir(), "invites.json")
	out, err := inviteCmd(p, []string{"add", "alice", "laptop"})
	if err != nil {
		t.Fatal(err)
	}
	code := strings.TrimSpace(out)
	inv, _ := readInvites(p)
	if !inv.valid(code) || inv[code].Label != "alice laptop" {
		t.Fatalf("add: %v", inv)
	}
	if l, _ := inviteCmd(p, []string{"list"}); !strings.Contains(l, code) || !strings.Contains(l, "active") {
		t.Fatalf("list: %q", l)
	}
	if _, err := inviteCmd(p, []string{"revoke", strings.ToLower(code)}); err != nil {
		t.Fatal(err)
	}
	inv, _ = readInvites(p)
	if inv.valid(code) {
		t.Fatal("still valid after revoke")
	}
	if l, _ := inviteCmd(p, []string{"list"}); !strings.Contains(l, "revoked") {
		t.Fatalf("list: %q", l)
	}
	if _, err := inviteCmd(p, []string{"revoke", "NOPE"}); err == nil {
		t.Fatal("revoke unknown should fail")
	}
}

func TestIdentity(t *testing.T) {
	dir := t.TempDir()
	sp := filepath.Join(dir, "secret.key")
	s1, err := loadSecret(sp)
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(sp); st.Mode().Perm() != 0o600 {
		t.Fatalf("secret mode %v", st.Mode())
	}
	s2, err := loadSecret(sp)
	if err != nil || string(s1) != string(s2) {
		t.Fatal("secret not stable", err)
	}
	os.Chmod(sp, 0o644)
	if _, err := loadSecret(sp); err == nil {
		t.Fatal("loose perms must be refused")
	}
	u1, sn1, m1 := identity(s1, "0123456789abcdef")
	u2, sn2, m2 := identity(s1, "0123456789abcdef")
	u3, _, _ := identity(s1, "fedcba9876543210")
	if u1 != u2 || sn1 != sn2 || m1 != m2 || u1 == u3 {
		t.Fatal("identity not deterministic/distinct")
	}
	if len(sn1) != 9 || !strings.HasPrefix(m1, "00:09:bf:") || len(m1) != 17 {
		t.Fatalf("format %s %s", sn1, m1)
	}
	if v := vipCandidate(s1, "0123456789abcdef", 0); v[0] != 10 || v[1] != 77 || v[2] == 0 || v[3] == 0 || v[2] == 255 {
		t.Fatalf("vip %v", v)
	}
	// vip collision: occupy candidate 0, expect candidate 1.
	g := newTestGW(t, nil, 0)
	dev := "0123456789abcdef"
	g.mu.Lock()
	g.hosts[vipCandidate(g.secret, dev, 0)] = &client{}
	g.mu.Unlock()
	srv := httptest.NewServer(g.Handler())
	defer srv.Close()
	c := dial(t, srv, dev, "x", testCode(t, g))
	v0, v1 := vipCandidate(g.secret, dev, 0), vipCandidate(g.secret, dev, 1)
	if c.hello["vip"] == net.IP(v0[:]).String() || c.hello["vip"] != net.IP(v1[:]).String() {
		t.Fatalf("collision not handled, got %s", c.hello["vip"])
	}
}

// ---- test harness ----

func newTestGW(t *testing.T, svc map[string]int, idle time.Duration) *Gateway {
	t.Helper()
	dir := t.TempDir()
	if _, err := inviteCmd(filepath.Join(dir, "invites.json"), []string{"add", "test"}); err != nil {
		t.Fatal(err)
	}
	g, err := NewGateway(Config{StateDir: dir, Services: svc, UDPIdle: idle})
	if err != nil {
		t.Fatal(err)
	}
	if idle > 0 {
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		go g.Run(ctx)
	}
	return g
}

func testCode(t *testing.T, g *Gateway) string {
	g.mu.RLock()
	defer g.mu.RUnlock()
	for c := range g.inv {
		return c
	}
	t.Fatal("no code")
	return ""
}

type tclient struct {
	t     *testing.T
	ws    *websocket.Conn
	hello map[string]string
}

func wsURL(srv *httptest.Server, dev, name, code string) string {
	return "ws" + strings.TrimPrefix(srv.URL, "http") + "/net?v=1&dev=" + dev + "&name=" + name + "&code=" + code
}

func dial(t *testing.T, srv *httptest.Server, dev, name, code string) *tclient {
	c, resp, err := tryDial(srv, dev, name, code)
	if err != nil {
		t.Fatalf("dial: %v %v", err, resp)
	}
	c.t = t
	t.Cleanup(func() { c.ws.CloseNow() })
	return c
}

func tryDial(srv *httptest.Server, dev, name, code string) (*tclient, *http.Response, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, resp, err := websocket.Dial(ctx, wsURL(srv, dev, name, code), &websocket.DialOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return nil, resp, err
	}
	ws.SetReadLimit(1 << 20)
	typ, b, err := ws.Read(ctx)
	if err != nil || typ != websocket.MessageText {
		return nil, resp, fmt.Errorf("hello: %v", err)
	}
	c := &tclient{ws: ws, hello: map[string]string{}}
	json.Unmarshal(b, &c.hello)
	return c, resp, nil
}

func (c *tclient) send(typ byte, id uint32, parts ...[]byte) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.ws.Write(ctx, websocket.MessageBinary, frame(typ, id, parts...)); err != nil {
		c.t.Errorf("send: %v", err)
	}
}

func (c *tclient) recv(d time.Duration) (typ byte, id uint32, p []byte, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	mt, b, err := c.ws.Read(ctx)
	if err != nil {
		return 0, 0, nil, err
	}
	if mt == websocket.MessageText {
		return 0xff, 0, b, nil
	}
	return b[0], binary.LittleEndian.Uint32(b[1:5]), b[5:], nil
}

func (c *tclient) expect(typ byte, id uint32) []byte {
	c.t.Helper()
	ty, i, p, err := c.recv(5 * time.Second)
	if err != nil || ty != typ || i != id {
		c.t.Fatalf("want type %#x id %d, got %#x id %d err %v", typ, id, ty, i, err)
	}
	return p
}

func ip4(s string) []byte { return net.ParseIP(s).To4() }

func (c *tclient) bind(id uint32, port uint16) uint16 {
	c.send(tUDPBind, id, le16(port))
	p := c.expect(tUDPBound, id)
	if p[0] != 0 {
		c.t.Fatalf("bind status %d", p[0])
	}
	return binary.LittleEndian.Uint16(p[1:])
}

func (c *tclient) udpSend(id uint32, ip string, port uint16, data string) {
	c.send(tUDPSend, id, ip4(ip), le16(port), []byte(data))
}

func (c *tclient) udpRecv(id uint32) (src string, sport uint16, data string) {
	p := c.expect(tUDPRecv, id)
	return net.IP(p[0:4]).String(), binary.LittleEndian.Uint16(p[4:6]), string(p[6:])
}

// fakeServices starts a TCP echo server and a UDP echo server ("from:<srcport>:<payload>") on loopback.
func fakeServices(t *testing.T) map[string]int {
	tl, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tl.Close() })
	svc := map[string]int{"80": tl.Addr().(*net.TCPAddr).Port}
	for _, gp := range []string{"27900", "27901"} {
		uc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { uc.Close() })
		svc[gp] = uc.LocalAddr().(*net.UDPAddr).Port
		go func() {
			buf := make([]byte, 4096)
			for {
				n, src, err := uc.ReadFromUDP(buf)
				if err != nil {
					return
				}
				uc.WriteToUDP([]byte(fmt.Sprintf("from:%d:%s", src.Port, buf[:n])), src)
			}
		}()
	}
	go func() {
		for {
			c, err := tl.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	return svc
}

func TestEndToEnd(t *testing.T) {
	g := newTestGW(t, fakeServices(t), 300*time.Millisecond)
	srv := httptest.NewServer(g.Handler())
	defer srv.Close()
	code := testCode(t, g)

	// invite / duplicate / bad request
	if _, resp, err := tryDial(srv, "aaaaaaaaaaaaaaaa", "a", "WRONG"); err == nil || resp.StatusCode != 403 {
		t.Fatalf("bad invite: %v %v", err, resp)
	}
	if _, resp, err := tryDial(srv, "zz", "a", code); err == nil || resp.StatusCode != 400 {
		t.Fatalf("bad dev: %v %v", err, resp)
	}
	a := dial(t, srv, "aaaaaaaaaaaaaaaa", "alice", strings.ToLower(code))
	b := dial(t, srv, "bbbbbbbbbbbbbbbb", "bob", code)
	if _, resp, err := tryDial(srv, "aaaaaaaaaaaaaaaa", "again", code); err == nil || resp.StatusCode != 409 {
		t.Fatalf("dup: %v %v", err, resp)
	}
	if a.hello["gw"] != "10.77.0.1" || a.hello["name"] != "alice" || a.hello["userId"] == "" || a.hello["vip"] == b.hello["vip"] {
		t.Fatalf("hello %v %v", a.hello, b.hello)
	}

	// DNS
	for i, h := range []string{"naswii.nintendowifi.net", "x.rwfc.net", "example.com"} {
		a.send(tDNSReq, uint32(i), []byte(h))
		p := a.expect(tDNSResp, uint32(i))
		if p[0] != 1 || net.IP(p[1:5]).String() != "10.77.0.1" {
			t.Fatalf("dns %s: %v", h, p)
		}
	}
	// PING
	a.send(tPing, 77, []byte("x"))
	if string(a.expect(tPing, 77)) != "x" {
		t.Fatal("ping")
	}

	// TCP through bridge (gw:80 -> echo), 100 KiB to exercise chunking; refused for unmapped, unreachable for foreign ip
	a.send(tTCPOpen, 5, ip4("10.77.0.1"), le16(80))
	if p := a.expect(tTCPOpened, 5); p[0] != 0 {
		t.Fatalf("tcp open %d", p[0])
	}
	big := strings.Repeat("0123456789abcdef", 6400)
	for i := 0; i < len(big); i += tcpChunk {
		a.send(tTCPData, 5, []byte(big[i:min(i+tcpChunk, len(big))]))
	}
	var got []byte
	for len(got) < len(big) {
		got = append(got, a.expect(tTCPData, 5)...)
	}
	if string(got) != big {
		t.Fatal("tcp echo mismatch")
	}
	a.send(tTCPClose, 5)
	a.send(tTCPOpen, 6, ip4("10.77.0.1"), le16(9999))
	if p := a.expect(tTCPOpened, 6); p[0] != 111 {
		t.Fatalf("want 111 got %d", p[0])
	}
	a.send(tTCPOpen, 7, ip4("8.8.8.8"), le16(80))
	if p := a.expect(tTCPOpened, 7); p[0] != 113 {
		t.Fatalf("want 113 got %d", p[0])
	}

	// UDP to service through the real socket; reply labelled gw:27900
	pa := a.bind(1, 0)
	pb := b.bind(1, 5555)
	if pb != 5555 {
		t.Fatalf("pb=%d", pb)
	}
	b.send(tUDPBind, 2, le16(5555))
	if p := b.expect(tUDPBound, 2); p[0] != 98 {
		t.Fatalf("dup bind status %d", p[0])
	}
	b.udpSend(1, "10.77.0.1", 27900, "hello")
	src, sport, data := b.udpRecv(1)
	if src != "10.77.0.1" || sport != 27900 || !strings.HasSuffix(data, ":hello") {
		t.Fatalf("service reply %s %d %s", src, sport, data)
	}
	var realB int
	fmt.Sscanf(data, "from:%d:", &realB)

	// peer by virtual form: a -> b at 10.77.x.y:5555
	a.udpSend(1, b.hello["vip"], 5555, "v-form")
	if src, sport, data := b.udpRecv(1); src != a.hello["vip"] || int(sport) != int(pa) || data != "v-form" {
		t.Fatalf("peer vform %s %d %s", src, sport, data)
	}
	// peer by real form: a -> 127.0.0.1:<b's real port>
	a.udpSend(1, "127.0.0.1", uint16(realB), "r-form")
	if src, sport, data := b.udpRecv(1); src != a.hello["vip"] || sport != pa || data != "r-form" {
		t.Fatalf("peer rform %s %d %s", src, sport, data)
	}
	// unknown destination: silently dropped (next recv must time out)
	a.udpSend(1, "10.77.9.9", 1, "nobody")
	a.send(tPing, 1)
	a.expect(tPing, 1) // a's frames are handled in order, so the unknown send is done
	b.send(tPing, 2)
	b.expect(tPing, 2) // would be a UDP_RECV first if the datagram had been delivered

	// idle cleanup frees the real port; socket still works afterwards
	time.Sleep(900 * time.Millisecond)
	g.mu.RLock()
	n := len(g.udpReal)
	g.mu.RUnlock()
	if n != 0 {
		t.Fatalf("idle real sockets not freed: %d", n)
	}
	b.udpSend(1, "10.77.0.1", 27901, "again")
	if _, sport, data := b.udpRecv(1); sport != 27901 || !strings.HasSuffix(data, ":again") {
		t.Fatalf("after idle: %d %s", sport, data)
	}

	// status
	resp, err := http.Get(srv.URL + "/status")
	if err != nil {
		t.Fatal(err)
	}
	var st struct {
		Count   int
		Players []struct{ Name, VIP string }
	}
	json.NewDecoder(resp.Body).Decode(&st)
	resp.Body.Close()
	if st.Count != 2 {
		t.Fatalf("status %+v", st)
	}

	// revoke -> kick, and rejoin refused; slot freed
	if _, err := inviteCmd(g.invPath, []string{"revoke", code}); err != nil {
		t.Fatal(err)
	}
	g.reloadInvites()
	ty, _, p, err := a.recv(3 * time.Second)
	if err != nil || ty != 0xff || !strings.Contains(string(p), `"kick"`) {
		t.Fatalf("kick: %#x %s %v", ty, p, err)
	}
	if _, _, _, err := a.recv(3 * time.Second); err == nil {
		t.Fatal("connection should close after kick")
	}
	if ty, _, _, err := b.recv(3 * time.Second); err != nil || ty != 0xff { // same code: b kicked too
		t.Fatalf("b kick: %v %v", ty, err)
	}
	b.recv(3 * time.Second)
	deadline := time.Now().Add(3 * time.Second)
	for {
		g.mu.RLock()
		n := len(g.devs)
		nv := len(g.udpVIP)
		g.mu.RUnlock()
		if n == 0 && nv == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not cleaned up: devs=%d udp=%d", n, nv)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, resp, err := tryDial(srv, "aaaaaaaaaaaaaaaa", "a", code); err == nil || resp.StatusCode != 403 {
		t.Fatalf("revoked code accepted: %v", err)
	}
}

func TestStorm12(t *testing.T) {
	const N, K = 12, 100
	g := newTestGW(t, fakeServices(t), 0)
	srv := httptest.NewServer(g.Handler())
	defer srv.Close()
	code := testCode(t, g)
	cs := make([]*tclient, N)
	ports := make([]uint16, N)
	for i := range cs {
		cs[i] = dial(t, srv, fmt.Sprintf("%016x", i+1), fmt.Sprintf("p%d", i), code)
		ports[i] = cs[i].bind(1, 0)
	}
	vipIdx := map[string]int{}
	for i, c := range cs {
		vipIdx[c.hello["vip"]] = i
	}
	var mu sync.Mutex
	recvd := make([][N]int, N) // recvd[dst][src]
	var wg sync.WaitGroup
	ctx, cancel := context.WithCancel(context.Background())
	for i := range cs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for {
				mt, b, err := cs[i].ws.Read(ctx)
				if err != nil {
					return
				}
				if mt != websocket.MessageBinary || b[0] != tUDPRecv {
					t.Errorf("client %d: unexpected frame %#x", i, b[0])
					return
				}
				srcip := net.IP(b[5:9]).String()
				sport := binary.LittleEndian.Uint16(b[9:11])
				var from, to, seq int
				fmt.Sscanf(string(b[11:]), "%d>%d#%d", &from, &to, &seq)
				if to != i || vipIdx[srcip] != from || sport != ports[from] {
					t.Errorf("cross-talk at %d: payload %q src %s:%d", i, b[11:], srcip, sport)
					return
				}
				mu.Lock()
				recvd[i][from]++
				mu.Unlock()
			}
		}(i)
	}
	var sw sync.WaitGroup
	for i := range cs {
		sw.Add(1)
		go func(i int) {
			defer sw.Done()
			for s := 0; s < K; s++ {
				for j := range cs {
					if j != i {
						cs[i].udpSend(1, cs[j].hello["vip"], ports[j], fmt.Sprintf("%d>%d#%d", i, j, s))
					}
				}
				time.Sleep(time.Millisecond)
			}
		}(i)
	}
	done := make(chan struct{})
	go func() { sw.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(40 * time.Second):
		t.Fatal("deadlock: senders stuck")
	}
	time.Sleep(500 * time.Millisecond)
	cancel()
	wg.Wait()
	total := 0
	for d := range recvd {
		for s := range recvd[d] {
			total += recvd[d][s]
			if s == d && recvd[d][s] != 0 {
				t.Errorf("self delivery %d", d)
			}
		}
	}
	want := N * (N - 1) * K
	t.Logf("delivered %d/%d", total, want)
	if total < want*8/10 {
		t.Fatalf("too much loss: %d/%d", total, want)
	}
}

func TestStaticRangeAndMime(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.wasm"), []byte("0123456789"), 0o644)
	os.WriteFile(filepath.Join(dir, "a.js"), []byte("console.log(1)"), 0o644)
	os.WriteFile(filepath.Join(dir, "a.js.gz"), []byte("GZ"), 0o644)
	os.WriteFile(filepath.Join(dir, "index.html"), []byte("<p>"), 0o644)
	srv := httptest.NewServer(static{root: dir})
	defer srv.Close()
	get := func(path string, h map[string]string) *http.Response {
		req, _ := http.NewRequest("GET", srv.URL+path, nil)
		for k, v := range h {
			req.Header.Set(k, v)
		}
		tr := &http.Transport{DisableCompression: true}
		r, err := (&http.Client{Transport: tr}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	r := get("/a.wasm", map[string]string{"Range": "bytes=2-4"})
	b, _ := io.ReadAll(r.Body)
	if r.StatusCode != 206 || string(b) != "234" || r.Header.Get("Content-Range") != "bytes 2-4/10" || r.Header.Get("Content-Type") != "application/wasm" {
		t.Fatalf("range: %d %q %v", r.StatusCode, b, r.Header)
	}
	if r := get("/a.wasm", map[string]string{"Range": "bytes=50-"}); r.StatusCode != 416 {
		t.Fatalf("416 expected, got %d", r.StatusCode)
	}
	r = get("/a.js", map[string]string{"Accept-Encoding": "gzip"})
	b, _ = io.ReadAll(r.Body)
	if string(b) != "GZ" || r.Header.Get("Content-Encoding") != "gzip" || r.Header.Get("Vary") != "Accept-Encoding" || r.Header.Get("Content-Length") != "2" {
		t.Fatalf("gz: %q %v", b, r.Header)
	}
	r = get("/a.js", map[string]string{"Accept-Encoding": "gzip", "Range": "bytes=0-2"})
	b, _ = io.ReadAll(r.Body)
	if string(b) != "con" || r.Header.Get("Content-Encoding") != "" {
		t.Fatalf("range must bypass gz: %q %v", b, r.Header)
	}
	if r := get("/../etc/passwd", nil); r.StatusCode != 404 {
		t.Fatalf("traversal %d", r.StatusCode)
	}
	if r := get("/", nil); r.StatusCode != 200 {
		t.Fatalf("index %d", r.StatusCode)
	}
}
