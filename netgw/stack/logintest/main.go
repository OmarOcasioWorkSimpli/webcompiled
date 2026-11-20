// logintest: headless client doing what the MKW runtime does against wfc-server, over loopback:
//   NAS POST /ac (acctcreate, login) -> GPCM TCP login (challenge/response) -> QR2 UDP heartbeat.
// Usage: go run ./logintest [-nas 127.0.0.1:18080] [-gp 127.0.0.1:29900] [-qr2 127.0.0.1:27900]
//                           [-userid N] [-wl bad] ; exit 0 = all stages passed.
package main

import (
	"bufio"
	"crypto/md5"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
	"unicode/utf16"
)

var dwc = base64.NewEncoding("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789.-").WithPadding('*')

var (
	nasAddr = flag.String("nas", "127.0.0.1:18080", "NAS HTTP (gateway maps virtual :80/:443 here)")
	gpAddr  = flag.String("gp", "127.0.0.1:29900", "GPCM")
	qrAddr  = flag.String("qr2", "127.0.0.1:27900", "QR2 UDP")
	userID  = flag.String("userid", "", "reuse an existing userid instead of acctcreate")
	wl      = flag.String("wl", "", `"bad": also send wl:ver + garbage wl:sig (shows signature rejection)`)
	name    = flag.String("name", "Tester", "ingamesn")
)

func fail(stage, f string, a ...any) {
	fmt.Printf("FAIL %s: %s\n", stage, fmt.Sprintf(f, a...))
	os.Exit(1)
}
func ok(stage, f string, a ...any) { fmt.Printf("ok   %-9s %s\n", stage, fmt.Sprintf(f, a...)) }

func utf16be(s string) []byte {
	var b []byte
	for _, u := range utf16.Encode([]rune(s)) {
		b = binary.BigEndian.AppendUint16(b, u)
	}
	return b
}

// nas posts DWC-base64 form fields like DWC_Nas does and returns the decoded response fields.
func nas(fields map[string][]byte) map[string]string {
	form := url.Values{}
	for k, v := range fields {
		form.Set(k, dwc.EncodeToString(v))
	}
	req, _ := http.NewRequest("POST", "http://"+*nasAddr+"/ac", strings.NewReader(form.Encode()))
	req.Host = "naswii.nintendowifi.net"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "RVL SDK/1.0")
	req.Header.Set("HTTP_X_GAMECD", "RMCP")
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		fail("nas", "%v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		fail("nas", "HTTP %d: %.100s", resp.StatusCode, body)
	}
	q, err := url.ParseQuery(strings.TrimRight(string(body), "\x00"))
	if err != nil {
		fail("nas", "parse: %v", err)
	}
	out := map[string]string{}
	for k := range q {
		v, err := dwc.DecodeString(strings.ReplaceAll(q.Get(k), " ", "+"))
		if err != nil {
			fail("nas", "decode %s: %v", k, err)
		}
		out[k] = string(v)
	}
	return out
}

func gsResp(gpChal, nasChal, token, clientChal string) string {
	m := md5.Sum([]byte(nasChal))
	h := hex.EncodeToString(m[:])
	s := md5.Sum([]byte(h + strings.Repeat(" ", 48) + token + clientChal + gpChal + h))
	return hex.EncodeToString(s[:])
}

// gpParse parses \k\v\k\v...\final\ into a map (first key = command).
func gpParse(msg string) map[string]string {
	p := strings.Split(strings.Trim(msg, "\\"), "\\")
	m := map[string]string{"_cmd": p[0], "_val": ""}
	if len(p) > 1 {
		m["_val"] = p[1]
	}
	for i := 2; i+1 < len(p); i += 2 {
		m[p[i]] = p[i+1]
	}
	return m
}

func rstr(n int) string {
	const a = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	for i := range b {
		b[i] = a[rand.Intn(len(a))]
	}
	return string(b)
}

func main() {
	flag.Parse()
	rand.Seed(time.Now().UnixNano())

	// --- NAS ---
	gsbrcd := "RMCP" + fmt.Sprintf("%08x", rand.Uint32())
	common := map[string][]byte{
		"gsbrcd": []byte(gsbrcd), "sdkver": []byte("0000000009"), "gamecd": []byte("RMCP"),
		"makercd": []byte("01"), "unitcd": []byte("1"), "macadr": []byte("0009bf123456"),
		"lang": []byte("01"), "devtime": []byte("221119103000"), "csnum": []byte("LU123456789"),
		"cfc": []byte("1234567890123456"), "region": []byte("02"), "ingamesn": utf16be(*name),
	}
	uid := *userID
	if uid == "" {
		f := map[string][]byte{"action": []byte("acctcreate")}
		for k, v := range common {
			f[k] = v
		}
		r := nas(f)
		if r["returncd"] != "002" || r["userid"] == "" {
			fail("acctcreate", "got %v", r)
		}
		uid = r["userid"]
		ok("acctcreate", "returncd=002 userid=%s", uid)
	}
	f := map[string][]byte{"action": []byte("login"), "userid": []byte(uid)}
	for k, v := range common {
		f[k] = v
	}
	r := nas(f)
	if r["returncd"] != "001" || r["token"] == "" || r["challenge"] == "" {
		fail("nas login", "returncd=%s (expected 001) %v", r["returncd"], r)
	}
	ok("nas login", "returncd=001 locator=%s challenge=%s token=%.12s...(%d chars)", r["locator"], r["challenge"], r["token"], len(r["token"]))
	token, nasChal := r["token"], r["challenge"]

	// --- GPCM ---
	c, err := net.DialTimeout("tcp", *gpAddr, 3*time.Second)
	if err != nil {
		fail("gpcm", "%v", err)
	}
	defer c.Close()
	rd := bufio.NewReader(c)
	readMsg := func() map[string]string {
		c.SetReadDeadline(time.Now().Add(5 * time.Second))
		var b []byte
		for !strings.HasSuffix(string(b), `\final\`) {
			x, err := rd.ReadByte()
			if err != nil {
				fail("gpcm", "read: %v (got %q)", err, b)
			}
			b = append(b, x)
		}
		return gpParse(string(b))
	}
	lc1 := readMsg()
	if lc1["_cmd"] != "lc" || lc1["_val"] != "1" {
		fail("gpcm", "expected lc 1, got %v", lc1)
	}
	cc := rstr(32) // DWC uses a 32-char client challenge
	login := fmt.Sprintf(`\login\\challenge\%s\authtoken\%s\partnerid\11\response\%s\firewall\1\port\0\productid\0\gamename\mariokartwii\namespaceid\0\sdkrevision\3\id\1`,
		cc, token, gsResp(lc1["challenge"], nasChal, token, cc))
	if *wl == "bad" {
		login += `\wl:ver\0x1000\wl:sig\` + dwc.EncodeToString(make([]byte, 0x144))
	}
	login += `\final\`
	c.Write([]byte(login))
	lc2 := readMsg()
	if lc2["_cmd"] == "error" {
		fail("gpcm login", "server error err=%s errmsg=%q wwfc=%q", lc2["err"], lc2["errmsg"], lc2["wl:msg"])
	}
	if lc2["_cmd"] != "lc" || lc2["_val"] != "2" {
		fail("gpcm login", "unexpected %v", lc2)
	}
	if want := gsResp(cc, nasChal, token, lc1["challenge"]); lc2["proof"] != want {
		fail("gpcm proof", "server proof %s != expected %s", lc2["proof"], want)
	}
	ok("gpcm", "lc2 profileid=%s userid=%s uniquenick=%s sesskey=%s proof OK", lc2["profileid"], lc2["userid"], lc2["uniquenick"], lc2["sesskey"])

	// --- QR2 (UDP) ---
	u, _ := net.ResolveUDPAddr("udp", *qrAddr)
	q, err := net.DialUDP("udp", nil, u)
	if err != nil {
		fail("qr2", "%v", err)
	}
	defer q.Close()
	sid := []byte{0x12, 0x34, 0x56, 0x78}
	kv := func(p ...string) []byte { return []byte(strings.Join(p, "\x00") + "\x00") }
	// AVAILABLE
	q.Write(append([]byte{0x09, 0, 0, 0, 0}, append([]byte("mariokartwii"), 0)...))
	buf := make([]byte, 1024)
	q.SetReadDeadline(time.Now().Add(3 * time.Second))
	if n, err := q.Read(buf); err != nil || n < 3 || buf[0] != 0xfe || buf[2] != 0x09 {
		fail("qr2", "no AVAILABLE reply: %v % x", err, buf[:n])
	}
	hb := append([]byte{0x03}, sid...)
	lp := q.LocalAddr().(*net.UDPAddr).Port
	hb = append(hb, kv("publicip", "2130706433", "publicport", fmt.Sprint(lp), "localip0", "10.77.0.2", "localport", fmt.Sprint(lp), "natneg", "1", "statechanged", "1", "gamename", "mariokartwii",
		"dwc_pid", lc2["profileid"], "dwc_mtype", "0", "dwc_hoststate", "0")...)
	q.Write(hb)
	// expect challenge (0xfe 0xfd 0x01 sid challenge\0); retried every 1s by server
	q.SetReadDeadline(time.Now().Add(4 * time.Second))
	n, err := q.Read(buf)
	if err != nil || n < 8 || buf[2] != 0x01 {
		fail("qr2", "no challenge after heartbeat: %v % x", err, buf[:n])
	}
	ok("qr2", "heartbeat accepted, challenge=%q", strings.TrimRight(string(buf[7:n]), "\x00"))
	q.Write(append([]byte{0x01}, append(sid, []byte("00000000")...)...)) // server does not verify the hash (TODO upstream)
	q.SetReadDeadline(time.Now().Add(4 * time.Second))
	for {
		n, err = q.Read(buf)
		if err != nil {
			fail("qr2", "no CLIENT_REGISTERED: %v", err)
		}
		if buf[2] == 0x0a {
			break
		}
	}
	ok("qr2", "CLIENT_REGISTERED received")
	// session shutdown
	q.Write(append([]byte{0x03}, append(sid, kv("statechanged", "2", "gamename", "mariokartwii")...)...))
	fmt.Println("PASS")
}
