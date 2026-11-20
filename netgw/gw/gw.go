package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/coder/websocket"
)

// Frame types (see PROTOCOL.md). Header: u8 type | u32 id (LE). All ints LE, IPv4 = 4 raw bytes (NBO).
const (
	tDNSReq    = 0x10
	tDNSResp   = 0x11
	tTCPOpen   = 0x20
	tTCPOpened = 0x21
	tTCPData   = 0x22
	tTCPClose  = 0x23
	tUDPBind   = 0x30
	tUDPBound  = 0x31
	tUDPSend   = 0x32
	tUDPRecv   = 0x33
	tUDPClose  = 0x34
	tPing      = 0x40

	maxUDPPayload = 2048
	tcpChunk      = 16 << 10
	maxQUDP       = 512  // queued UDP datagrams per client; oldest dropped beyond
	maxQOther     = 1024 // queued TCP/control frames per client; producers block beyond
)

var (
	gwIP   = [4]byte{10, 77, 0, 1}
	loopIP = [4]byte{127, 0, 0, 1}
	devRE  = regexp.MustCompile(`^[0-9a-f]{16}$`)
)

func frame(t byte, id uint32, parts ...[]byte) []byte {
	b := make([]byte, 5, 64)
	b[0] = t
	binary.LittleEndian.PutUint32(b[1:], id)
	for _, p := range parts {
		b = append(b, p...)
	}
	return b
}

func le16(v uint16) []byte { return binary.LittleEndian.AppendUint16(nil, v) }

// ---- per-client outbound queue: bounded, drop-oldest for UDP, backpressure for the rest ----

type qitem struct {
	b   []byte
	udp bool
}

type queue struct {
	mu     sync.Mutex
	cond   *sync.Cond
	items  []qitem
	nudp   int
	closed bool
}

func newQueue() *queue { q := &queue{}; q.cond = sync.NewCond(&q.mu); return q }

func (q *queue) push(b []byte, udp bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if udp {
		if q.nudp >= maxQUDP {
			for i, it := range q.items {
				if it.udp {
					q.items = append(q.items[:i], q.items[i+1:]...)
					q.nudp--
					break
				}
			}
		}
		q.nudp++
	} else {
		for len(q.items)-q.nudp >= maxQOther && !q.closed {
			q.cond.Wait()
		}
	}
	if q.closed {
		return
	}
	q.items = append(q.items, qitem{b, udp})
	q.cond.Broadcast()
}

func (q *queue) pop() ([]byte, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.items) == 0 && !q.closed {
		q.cond.Wait()
	}
	if q.closed {
		return nil, false
	}
	it := q.items[0]
	q.items[0] = qitem{}
	q.items = q.items[1:]
	if it.udp {
		q.nudp--
	}
	q.cond.Broadcast()
	return it.b, true
}

func (q *queue) close() {
	q.mu.Lock()
	q.closed = true
	q.cond.Broadcast()
	q.mu.Unlock()
}

// ---- gateway ----

type Config struct {
	StateDir   string
	Services   map[string]int // "80": 8080, ... gw port (as string) -> loopback port (TCP and UDP)
	UDPIdle    time.Duration  // close a socket's real UDP port after this long without traffic
	AdminToken string
	Log        *log.Logger
}

type ap struct {
	ip   [4]byte
	port uint16
}

type Gateway struct {
	cfg     Config
	secret  []byte
	invPath string
	udpRev  map[int]uint16 // loopback service port -> gw port, to label replies

	mu       sync.RWMutex // guards everything below. Lock order: usock.mu before g.mu.
	inv      Invites
	invStamp [2]int64
	devs     map[string]*client
	hosts    map[[4]byte]*client
	udpVIP   map[ap]*usock
	udpReal  map[uint16]*usock
}

func NewGateway(cfg Config) (*Gateway, error) {
	if cfg.Log == nil {
		cfg.Log = log.New(os.Stderr, "netgw ", log.LstdFlags)
	}
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return nil, err
	}
	secret, err := loadSecret(cfg.StateDir + "/secret.key")
	if err != nil {
		return nil, err
	}
	g := &Gateway{cfg: cfg, secret: secret, invPath: cfg.StateDir + "/invites.json",
		devs: map[string]*client{}, hosts: map[[4]byte]*client{},
		udpVIP: map[ap]*usock{}, udpReal: map[uint16]*usock{}, udpRev: map[int]uint16{}}
	for k, v := range cfg.Services {
		var p int
		if _, err := fmt.Sscanf(strings.TrimPrefix(k, "udp/"), "%d", &p); err == nil {
			g.udpRev[v] = uint16(p)
		}
	}
	g.reloadInvites()
	return g, nil
}

// udpTarget maps a gw UDP port to the loopback service port. QR2/NATNEG default to themselves.
func (g *Gateway) udpTarget(port uint16) (int, bool) {
	if v, ok := g.cfg.Services["udp/"+itoa(int(port))]; ok {
		return v, true
	}
	if v, ok := g.cfg.Services[itoa(int(port))]; ok {
		return v, true
	}
	if port == 27900 || port == 27901 {
		return int(port), true
	}
	return 0, false
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [8]byte
	n := len(b)
	for ; i > 0; i /= 10 {
		n--
		b[n] = byte('0' + i%10)
	}
	return string(b[n:])
}

// reloadInvites re-reads invites.json if it changed, then kicks clients whose code is no longer valid.
func (g *Gateway) reloadInvites() {
	st, err := os.Stat(g.invPath)
	var stamp [2]int64
	if err == nil {
		stamp = [2]int64{st.ModTime().UnixNano(), st.Size()}
	}
	g.mu.RLock()
	same := stamp == g.invStamp && g.inv != nil
	g.mu.RUnlock()
	if same {
		return
	}
	inv, err := readInvites(g.invPath)
	if err != nil {
		g.cfg.Log.Printf("invites reload: %v (keeping old)", err)
		return
	}
	g.mu.Lock()
	g.inv, g.invStamp = inv, stamp
	var bad []*client
	for _, c := range g.devs {
		if !inv.valid(c.code) {
			bad = append(bad, c)
		}
	}
	g.mu.Unlock()
	for _, c := range bad {
		go c.kick("invite revoked")
	}
}

func (inv Invites) valid(code string) bool {
	i, ok := inv[code]
	return ok && !i.Revoked
}

func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/net", g.serveNet)
	mux.HandleFunc("/status", g.serveStatus)
	return mux
}

func (g *Gateway) Run(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	idle := g.cfg.UDPIdle
	sweep := time.NewTicker(max(idle/4, 20*time.Millisecond))
	defer sweep.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			g.reloadInvites()
		case <-sweep.C:
			if idle > 0 {
				g.sweepUDP(idle)
			}
		}
	}
}

func (g *Gateway) sweepUDP(idle time.Duration) {
	g.mu.RLock()
	socks := make([]*usock, 0, len(g.udpVIP))
	for _, s := range g.udpVIP {
		socks = append(socks, s)
	}
	g.mu.RUnlock()
	now := time.Now().UnixNano()
	for _, s := range socks {
		if now-s.last.Load() < int64(idle) {
			continue
		}
		s.mu.Lock()
		if s.real != nil {
			s.releaseReal()
		}
		s.mu.Unlock()
	}
}

func (g *Gateway) serveStatus(w http.ResponseWriter, r *http.Request) {
	if g.cfg.AdminToken != "" {
		if tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "); tok != g.cfg.AdminToken && r.URL.Query().Get("token") != g.cfg.AdminToken {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
	} else if host, _, _ := net.SplitHostPort(r.RemoteAddr); !net.ParseIP(host).IsLoopback() {
		http.Error(w, "forbidden (loopback only; set --admin-token)", http.StatusForbidden)
		return
	}
	type pl struct {
		Name  string    `json:"name"`
		VIP   string    `json:"vip"`
		Since time.Time `json:"since"`
	}
	g.mu.RLock()
	ps := make([]pl, 0, len(g.devs))
	udp := len(g.udpVIP)
	for _, c := range g.devs {
		ps = append(ps, pl{c.name, net.IP(c.vip[:]).String(), c.since})
	}
	nin := len(g.inv)
	g.mu.RUnlock()
	sort.Slice(ps, func(i, j int) bool { return ps[i].Since.Before(ps[j].Since) })
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"players": ps, "count": len(ps), "udp_sockets": udp, "invites": nin})
}

// ---- client ----

type client struct {
	g      *Gateway
	ws     *websocket.Conn
	dev    string
	name   string
	code   string
	vip    [4]byte
	since  time.Time
	ctx    context.Context
	cancel context.CancelFunc
	q      *queue
	ready  chan struct{} // closed once ws is set

	mu   sync.Mutex
	tcps map[uint32]*tcpConn
	udps map[uint32]*usock
}

func cleanName(s string) string {
	r := []rune(strings.TrimSpace(s))
	out := r[:0]
	for _, c := range r {
		if c >= 32 && c != 127 {
			out = append(out, c)
		}
	}
	if len(out) > 32 {
		out = out[:32]
	}
	if len(out) == 0 {
		return "player"
	}
	return string(out)
}

func (g *Gateway) serveNet(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	dev := q.Get("dev")
	if q.Get("v") != "1" || !devRE.MatchString(dev) {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	g.reloadInvites()
	code := normCode(q.Get("code"))
	ctx, cancel := context.WithCancel(context.Background())
	c := &client{g: g, dev: dev, name: cleanName(q.Get("name")), code: code, since: time.Now(),
		ctx: ctx, cancel: cancel, q: newQueue(), ready: make(chan struct{}), tcps: map[uint32]*tcpConn{}, udps: map[uint32]*usock{}}
	defer cancel()

	g.mu.Lock()
	if !g.inv.valid(code) {
		g.mu.Unlock()
		http.Error(w, "invalid invite", http.StatusForbidden)
		return
	}
	if _, dup := g.devs[dev]; dup {
		g.mu.Unlock()
		http.Error(w, "device already connected", http.StatusConflict)
		return
	}
	for n := 0; ; n++ { // collision handling: next hash candidate until the vip is free
		v := vipCandidate(g.secret, dev, n)
		if _, used := g.hosts[v]; !used {
			c.vip = v
			break
		}
	}
	g.devs[dev], g.hosts[c.vip] = c, c
	g.mu.Unlock()
	defer c.cleanup()

	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		CompressionMode: websocket.CompressionDisabled,
		OriginPatterns:  []string{"*"}, // LAN tool; the invite code is the gate
	})
	if err != nil {
		return
	}
	c.ws = ws
	close(c.ready)
	ws.SetReadLimit(1 << 16)
	uid, serno, macAddr := identity(g.secret, dev)
	hello, _ := json.Marshal(map[string]string{"vip": net.IP(c.vip[:]).String(), "gw": "10.77.0.1",
		"name": c.name, "userId": uid, "serno": serno, "mac": macAddr})
	wctx, wc := context.WithTimeout(ctx, 10*time.Second)
	err = ws.Write(wctx, websocket.MessageText, hello)
	wc()
	if err != nil {
		ws.CloseNow()
		return
	}
	g.cfg.Log.Printf("join %s vip=%s dev=%s", c.name, net.IP(c.vip[:]), dev)
	go c.writeLoop()
	c.readLoop()
	g.cfg.Log.Printf("leave %s vip=%s", c.name, net.IP(c.vip[:]))
}

func (c *client) writeLoop() {
	for {
		b, ok := c.q.pop()
		if !ok {
			return
		}
		ctx, cancel := context.WithTimeout(c.ctx, 15*time.Second)
		err := c.ws.Write(ctx, websocket.MessageBinary, b)
		cancel()
		if err != nil {
			c.ws.CloseNow()
			c.cancel()
			return
		}
	}
}

func (c *client) kick(reason string) {
	select {
	case <-c.ready:
	case <-c.ctx.Done():
		return
	}
	ctx, cancel := context.WithTimeout(c.ctx, 2*time.Second)
	defer cancel()
	msg, _ := json.Marshal(map[string]string{"evt": "kick", "reason": reason})
	c.ws.Write(ctx, websocket.MessageText, msg)
	c.ws.Close(websocket.StatusPolicyViolation, reason)
}

func (c *client) cleanup() {
	c.cancel()
	c.q.close()
	if c.ws != nil {
		c.ws.CloseNow()
	}
	c.mu.Lock()
	tcps, udps := c.tcps, c.udps
	c.tcps, c.udps = map[uint32]*tcpConn{}, map[uint32]*usock{}
	c.mu.Unlock()
	for _, t := range tcps {
		t.close()
	}
	for _, s := range udps {
		s.close()
	}
	c.g.mu.Lock()
	delete(c.g.devs, c.dev)
	delete(c.g.hosts, c.vip)
	c.g.mu.Unlock()
}

func (c *client) readLoop() {
	for {
		typ, data, err := c.ws.Read(c.ctx)
		if err != nil {
			if websocket.CloseStatus(err) == -1 && c.ctx.Err() == nil { // oversize/protocol error: say why, not a bare 1006
				c.ws.Close(websocket.StatusMessageTooBig, "frame too large or invalid")
			}
			return
		}
		if typ != websocket.MessageBinary || len(data) < 5 {
			c.ws.Close(websocket.StatusUnsupportedData, "binary frames of at least 5 bytes only")
			return
		}
		c.handle(data[0], binary.LittleEndian.Uint32(data[1:5]), data[5:])
	}
}

func (c *client) send(t byte, id uint32, parts ...[]byte) { c.q.push(frame(t, id, parts...), false) }

func (c *client) handle(t byte, id uint32, p []byte) {
	if os.Getenv("NETGW_TRACE") != "" { // ponytail: debug aid
		n := len(p)
		if n > 48 {
			n = 48
		}
		log.Printf("rx %s t=%#x id=%d len=%d %q", c.name, t, id, len(p), p[:n])
	}
	switch t {
	case tDNSReq: // everything resolves to the gateway; real DNS is never used
		c.send(tDNSResp, id, []byte{1}, gwIP[:])
	case tPing:
		c.send(tPing, id, p)
	case tTCPOpen:
		if len(p) >= 6 {
			c.openTCP(id, [4]byte(p[0:4]), binary.LittleEndian.Uint16(p[4:6]))
		}
	case tTCPData:
		c.mu.Lock()
		tc := c.tcps[id]
		c.mu.Unlock()
		if tc != nil {
			select {
			case tc.out <- p:
			default: // peer service not draining: drop the connection rather than stall everyone
				c.q.push(frame(tTCPClose, id), false)
				tc.close()
			}
		}
	case tTCPClose:
		c.mu.Lock()
		tc := c.tcps[id]
		delete(c.tcps, id)
		c.mu.Unlock()
		if tc != nil {
			tc.close()
		}
	case tUDPBind:
		if len(p) >= 2 {
			c.bindUDP(id, binary.LittleEndian.Uint16(p))
		}
	case tUDPSend:
		if len(p) >= 6 && len(p)-6 <= maxUDPPayload {
			c.mu.Lock()
			s := c.udps[id]
			c.mu.Unlock()
			if s != nil {
				s.send([4]byte(p[0:4]), binary.LittleEndian.Uint16(p[4:6]), p[6:])
			}
		}
	case tUDPClose:
		c.mu.Lock()
		s := c.udps[id]
		delete(c.udps, id)
		c.mu.Unlock()
		if s != nil {
			s.close()
		}
	}
}

// ---- TCP bridge ----

type tcpConn struct {
	out  chan []byte
	done chan struct{}
	once sync.Once
}

func (t *tcpConn) close() { t.once.Do(func() { close(t.done) }) }

const maxPerClient = 32 // TCP conns and UDP sockets each

func (c *client) openTCP(id uint32, ip [4]byte, port uint16) {
	tc := &tcpConn{out: make(chan []byte, 256), done: make(chan struct{})}
	c.mu.Lock()
	old := c.tcps[id]
	if old == nil && len(c.tcps) >= maxPerClient { // cap fds per client
		c.mu.Unlock()
		c.send(tTCPOpened, id, []byte{24}) // EMFILE
		return
	}
	c.tcps[id] = tc
	c.mu.Unlock()
	if old != nil {
		old.close()
	}
	go func() {
		status := c.dialStatus(id, tc, ip, port)
		if status != 0 {
			c.mu.Lock()
			if c.tcps[id] == tc {
				delete(c.tcps, id)
			}
			c.mu.Unlock()
			c.send(tTCPOpened, id, []byte{status})
		}
	}()
}

func (c *client) dialStatus(id uint32, tc *tcpConn, ip [4]byte, port uint16) byte {
	if ip != gwIP && ip != loopIP {
		return 113
	}
	target, ok := c.g.cfg.Services[itoa(int(port))]
	if !ok {
		return 111
	}
	d := net.Dialer{Timeout: 5 * time.Second}
	conn, err := d.DialContext(c.ctx, "tcp4", "127.0.0.1:"+itoa(target))
	if err != nil {
		var ne net.Error
		switch {
		case errors.Is(err, syscall.ECONNREFUSED):
			return 111
		case errors.As(err, &ne) && ne.Timeout():
			return 110
		}
		return 113
	}
	select {
	case <-tc.done: // closed while dialing
		conn.Close()
		return 0
	default:
	}
	c.send(tTCPOpened, id, []byte{0})
	go func() { // wfc-server -> client
		buf := make([]byte, tcpChunk)
		for {
			n, err := conn.Read(buf)
			if n > 0 {
				c.q.push(frame(tTCPData, id, buf[:n]), false)
			}
			if err != nil {
				select {
				case <-tc.done: // client already closed it
				default:
					c.q.push(frame(tTCPClose, id), false)
					tc.close()
				}
				return
			}
		}
	}()
	go func() { // client -> wfc-server
		defer conn.Close()
		for {
			select {
			case b := <-tc.out:
				if _, err := conn.Write(b); err != nil {
					c.q.push(frame(tTCPClose, id), false)
					tc.close()
					return
				}
			case <-tc.done: // flush what the guest sent just before closing
				for {
					select {
					case b := <-tc.out:
						conn.Write(b)
					default:
						return
					}
				}
			case <-c.ctx.Done():
				return
			}
		}
	}()
	return 0
}

// ---- UDP ----

type usock struct {
	c      *client
	id     uint32
	port   uint16 // the port the guest bound (virtual)
	last   atomic.Int64
	mu     sync.Mutex
	real   *net.UDPConn // lazily created 127.0.0.1:<ephemeral>; dropped when idle
	rport  uint16
	closed bool
}

func (s *usock) touch() { s.last.Store(time.Now().UnixNano()) }

func (c *client) bindUDP(id uint32, port uint16) {
	g := c.g
	c.mu.Lock()
	_, exists := c.udps[id]
	full := len(c.udps) >= maxPerClient
	c.mu.Unlock()
	reply := func(status byte, p uint16) { c.send(tUDPBound, id, []byte{status}, le16(p)) }
	if exists {
		reply(22, 0)
		return
	}
	if full {
		reply(24, 0) // EMFILE: cap sockets per client
		return
	}
	s := &usock{c: c, id: id}
	s.touch()
	g.mu.Lock()
	if port == 0 {
		for i := 0; i < 100 && port == 0; i++ {
			p := uint16(49152 + rand.Intn(65536-49152))
			if _, used := g.udpVIP[ap{c.vip, p}]; !used {
				port = p
			}
		}
	}
	if _, used := g.udpVIP[ap{c.vip, port}]; used || port == 0 {
		g.mu.Unlock()
		reply(98, 0) // EADDRINUSE
		return
	}
	s.port = port
	g.udpVIP[ap{c.vip, port}] = s
	g.mu.Unlock()
	c.mu.Lock()
	c.udps[id] = s
	c.mu.Unlock()
	reply(0, port)
}

// conn returns the real loopback socket, creating it (and registering its port for peer lookup) on first use.
func (s *usock) conn() *net.UDPConn {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	if s.real == nil {
		uc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IP(loopIP[:])})
		if err != nil {
			return nil
		}
		s.real, s.rport = uc, uint16(uc.LocalAddr().(*net.UDPAddr).Port)
		s.c.g.mu.Lock()
		s.c.g.udpReal[s.rport] = s
		s.c.g.mu.Unlock()
		go s.readLoop(uc)
	}
	return s.real
}

func (s *usock) releaseReal() { // s.mu held
	s.real.Close()
	s.c.g.mu.Lock()
	delete(s.c.g.udpReal, s.rport)
	s.c.g.mu.Unlock()
	s.real = nil
}

func (s *usock) close() {
	s.mu.Lock()
	s.closed = true
	if s.real != nil {
		s.releaseReal()
	}
	s.mu.Unlock()
	s.c.g.mu.Lock()
	delete(s.c.g.udpVIP, ap{s.c.vip, s.port})
	s.c.g.mu.Unlock()
}

// readLoop: replies from wfc-server (service ports) come back as UDP_RECV from gw:<service port>.
func (s *usock) readLoop(uc *net.UDPConn) {
	buf := make([]byte, 4096)
	for {
		n, src, err := uc.ReadFromUDP(buf)
		if err != nil {
			return
		}
		s.touch()
		sip, sport := gwIP, uint16(src.Port)
		if v, ok := s.c.g.udpRev[src.Port]; ok {
			sport = v
		} else if ip4 := src.IP.To4(); ip4 != nil && !ip4.IsLoopback() {
			sip = [4]byte(ip4)
		}
		s.c.q.push(frame(tUDPRecv, s.id, sip[:], le16(sport), buf[:n]), true)
	}
}

func (s *usock) send(ip [4]byte, port uint16, payload []byte) {
	s.touch()
	g := s.c.g
	if ip == loopIP {
		g.mu.RLock()
		peer := g.udpReal[port]
		g.mu.RUnlock()
		if peer != nil {
			s.deliver(peer, payload)
			return
		}
	}
	if ip == gwIP || ip == loopIP {
		if tgt, ok := g.udpTarget(port); ok {
			if uc := s.conn(); uc != nil {
				uc.WriteToUDP(payload, &net.UDPAddr{IP: net.IP(loopIP[:]), Port: tgt})
			}
		}
		return
	}
	g.mu.RLock()
	peer := g.udpVIP[ap{ip, port}]
	g.mu.RUnlock()
	if peer != nil {
		s.deliver(peer, payload)
	} // else: unknown destination, dropped like real UDP
}

func (s *usock) deliver(dst *usock, payload []byte) {
	dst.touch()
	dst.c.q.push(frame(tUDPRecv, dst.id, s.c.vip[:], le16(s.port), payload), true)
}
