// loadgen: N fake clients, all-to-all UDP through the gateway; reports loss and latency.
//
//	loadgen -url ws://127.0.0.1:8080/net -code ABCD-EFGH -n 12 -rate 60 -dur 10s
//
// -rate is datagrams per second sent by each client to EACH peer. Latency = send->receive on the
// same host clock (payload carries a monotonic-ish ns timestamp), so run it on one machine.
package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

func frame(t byte, id uint32, parts ...[]byte) []byte {
	b := binary.LittleEndian.AppendUint32([]byte{t}, id)
	for _, p := range parts {
		b = append(b, p...)
	}
	return b
}

type cl struct {
	ws   *websocket.Conn
	vip  net.IP
	port uint16
}

func main() {
	u := flag.String("url", "ws://127.0.0.1:8080/net", "gateway /net URL")
	code := flag.String("code", "", "invite code")
	n := flag.Int("n", 12, "clients")
	rate := flag.Float64("rate", 60, "datagrams/s from each client to each peer")
	size := flag.Int("size", 100, "payload bytes (>=24)")
	dur := flag.Duration("dur", 10*time.Second, "send duration")
	flag.Parse()
	if *size < 24 {
		*size = 24
	}
	ctx := context.Background()
	cs := make([]*cl, *n)
	for i := range cs {
		q := url.Values{"v": {"1"}, "dev": {fmt.Sprintf("%016x", 0x10adc0de0000+i)}, "name": {fmt.Sprintf("load%d", i)}, "code": {*code}}
		ws, _, err := websocket.Dial(ctx, *u+"?"+q.Encode(), &websocket.DialOptions{CompressionMode: websocket.CompressionDisabled})
		if err != nil {
			fmt.Fprintf(os.Stderr, "client %d: %v\n", i, err)
			os.Exit(1)
		}
		ws.SetReadLimit(1 << 20)
		c := &cl{ws: ws}
		_, hello, err := ws.Read(ctx)
		if err != nil {
			fmt.Fprintln(os.Stderr, "hello:", err)
			os.Exit(1)
		}
		var h struct{ VIP string }
		json.Unmarshal(hello, &h)
		c.vip = net.ParseIP(h.VIP).To4()
		ws.Write(ctx, websocket.MessageBinary, frame(0x30, 1, []byte{0, 0}))
		_, b, err := ws.Read(ctx)
		if err != nil || len(b) < 8 || b[0] != 0x31 || b[5] != 0 {
			fmt.Fprintln(os.Stderr, "bind failed")
			os.Exit(1)
		}
		c.port = binary.LittleEndian.Uint16(b[6:8])
		cs[i] = c
	}

	var recv, sent atomic.Int64
	var mu sync.Mutex
	var lat []time.Duration
	rctx, stop := context.WithCancel(ctx)
	var rw sync.WaitGroup
	for _, c := range cs {
		rw.Add(1)
		go func(c *cl) {
			defer rw.Done()
			var local []time.Duration
			for {
				_, b, err := c.ws.Read(rctx)
				if err != nil {
					break
				}
				if len(b) >= 11+16 && b[0] == 0x33 {
					ts := int64(binary.LittleEndian.Uint64(b[11+8:]))
					local = append(local, time.Duration(time.Now().UnixNano()-ts))
					recv.Add(1)
				}
			}
			mu.Lock()
			lat = append(lat, local...)
			mu.Unlock()
		}(c)
	}
	interval := time.Duration(float64(time.Second) / *rate)
	var sw sync.WaitGroup
	end := time.Now().Add(*dur)
	for i, c := range cs {
		sw.Add(1)
		go func(i int, c *cl) {
			defer sw.Done()
			buf := make([]byte, *size)
			var seq uint64
			next := time.Now()
			for time.Now().Before(end) {
				for j, p := range cs {
					if j == i {
						continue
					}
					binary.LittleEndian.PutUint64(buf[0:], seq)
					binary.LittleEndian.PutUint64(buf[8:], uint64(time.Now().UnixNano()))
					if c.ws.Write(ctx, websocket.MessageBinary, frame(0x32, 1, p.vip, binary.LittleEndian.AppendUint16(nil, p.port), buf)) == nil {
						sent.Add(1)
					}
				}
				seq++
				next = next.Add(interval)
				time.Sleep(time.Until(next))
			}
		}(i, c)
	}
	sw.Wait()
	time.Sleep(time.Second)
	stop()
	rw.Wait()
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	pct := func(p float64) time.Duration {
		if len(lat) == 0 {
			return 0
		}
		return lat[min(int(float64(len(lat))*p), len(lat)-1)]
	}
	s, r := sent.Load(), recv.Load()
	fmt.Printf("clients=%d sent=%d recv=%d loss=%.2f%% rate=%.0f dgram/s total  p50=%v p99=%v max=%v\n",
		*n, s, r, 100*float64(s-r)/float64(max(s, 1)), float64(s)/dur.Seconds(), pct(.5), pct(.99), pct(1))
}
