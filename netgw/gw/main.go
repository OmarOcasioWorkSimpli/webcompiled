// netgw: static server + /net WebSocket gateway for the WebCompiled private online mode. See ../PROTOCOL.md.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"time"
)

func loadServices(path string) (map[string]int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	m := map[string]int{}
	return m, json.Unmarshal(b, &m)
}

func main() {
	exe, _ := os.Executable()
	defServices := filepath.Join(filepath.Dir(exe), "..", "services.json")
	fs := flag.NewFlagSet("netgw", flag.ExitOnError)
	dir := fs.String("dir", "/Users/omarocasio/Desktop/KartCloneBrowser/webcompiled/web/dist", "directory to serve")
	httpAddr := fs.String("http", ":8080", "plain HTTP listen address ('' to disable)")
	httpsAddr := fs.String("https", ":8443", "HTTPS listen address (only with --cert/--key)")
	cert := fs.String("cert", "", "TLS certificate PEM")
	key := fs.String("key", "", "TLS key PEM")
	state := fs.String("state", "./state", "state dir (secret.key, invites.json)")
	services := fs.String("services", defServices, "services.json (gw port -> loopback port)")
	idle := fs.Duration("udp-idle", 5*time.Minute, "free a UDP socket's real port after this idle time")
	admin := fs.String("admin-token", "", "token for /status (default: loopback clients only)")

	args := os.Args[1:]
	if len(args) > 0 && args[0] == "invite" {
		fs.Parse(args[1:])
		out, err := inviteCmd(filepath.Join(*state, "invites.json"), fs.Args())
		if err != nil {
			fmt.Fprintln(os.Stderr, "invite:", err)
			os.Exit(1)
		}
		fmt.Print(out)
		return
	}
	fs.Parse(args)

	svc, err := loadServices(*services)
	if err != nil {
		log.Fatalf("services: %v", err)
	}
	g, err := NewGateway(Config{StateDir: *state, Services: svc, UDPIdle: *idle, AdminToken: *admin})
	if err != nil {
		log.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/net", g.Handler())
	mux.Handle("/status", g.Handler())
	mux.Handle("/", static{root: *dir})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	go g.Run(ctx)
	errc := make(chan error, 2)
	if *httpAddr != "" {
		log.Printf("http %s serving %s", *httpAddr, *dir)
		go func() { errc <- (&http.Server{Addr: *httpAddr, Handler: mux}).ListenAndServe() }()
	}
	if *cert != "" && *key != "" {
		log.Printf("https %s", *httpsAddr)
		go func() { errc <- (&http.Server{Addr: *httpsAddr, Handler: mux}).ListenAndServeTLS(*cert, *key) }()
	}
	select {
	case err := <-errc:
		log.Fatal(err)
	case <-ctx.Done():
	}
}
