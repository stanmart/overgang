// Command overgang emulates an IKEA TRÅDFRI gateway on top of Home Assistant, so the old
// TRÅDFRI app keeps working after the devices move elsewhere.
//
//	overgang serve  -config overgang.toml           run the emulated gateway
//	overgang client -addr host:5684 -code … dump    read a gateway (real or emulated)
//	overgang proxy  -gateway host:5684 -code …      log the app talking to a real gateway
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/stanmart/overgang/internal/backend"
	"github.com/stanmart/overgang/internal/backend/fake"
	"github.com/stanmart/overgang/internal/backend/homeassistant"
	"github.com/stanmart/overgang/internal/bridge"
	"github.com/stanmart/overgang/internal/client"
	"github.com/stanmart/overgang/internal/config"
	"github.com/stanmart/overgang/internal/gateway"
	"github.com/stanmart/overgang/internal/mdns"
	"github.com/stanmart/overgang/internal/proxy"
	"github.com/stanmart/overgang/internal/state"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(ctx, os.Args[2:])
	case "client":
		err = runClient(ctx, os.Args[2:])
	case "proxy":
		err = runProxy(ctx, os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		slog.Error(err.Error())
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: overgang serve|client|proxy [flags]; see -h on each")
	os.Exit(2)
}

func setupLog(verbose bool) {
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
}

func serve(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	path := fs.String("config", "overgang.toml", "config file")
	verbose := fs.Bool("v", false, "log every request")
	fs.Parse(args)
	setupLog(*verbose)

	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	store, err := state.Open(cfg.Gateway.StateFile)
	if err != nil {
		return err
	}

	var be backend.Backend
	switch cfg.Backend.Kind {
	case "fake":
		modes := map[string][]string{}
		for _, r := range cfg.Rooms {
			for _, d := range r.Devices {
				modes[d.Entity] = d.FakeColorModes
			}
		}
		be = fake.New(modes)
	case "homeassistant":
		be = homeassistant.New(cfg.Backend.URL, cfg.Backend.Token)
	}

	br, err := bridge.New(cfg.Rooms, be, store)
	if err != nil {
		return err
	}
	gatewayID := strings.ToLower(strings.NewReplacer(":", "", "-", "").Replace(cfg.Gateway.MAC))
	gw := gateway.New(gateway.Config{
		Listen:       cfg.Gateway.Listen,
		SecurityCode: cfg.Gateway.SecurityCode,
		Firmware:     cfg.Gateway.Firmware,
		GatewayID:    gatewayID,
		Resources:    resources(cfg.Gateway.Resources),
	}, br, store)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	errc := make(chan error, 3)
	run := func(f func() error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := f(); err != nil {
				errc <- err
				cancel()
			}
		}()
	}
	ready := make(chan struct{})
	run(func() error { return be.Run(ctx, br.Watch(), br.Update) })
	run(func() error { return gw.Run(ctx, ready) })
	if cfg.Gateway.MDNS {
		run(func() error {
			select {
			case <-ready:
			case <-ctx.Done():
				return nil
			}
			_, port, _ := net.SplitHostPort(gw.Addr())
			p, _ := strconv.Atoi(port)
			return mdns.Announce(ctx, gatewayID, p, cfg.Gateway.Firmware)
		})
	}
	wg.Wait()
	close(errc)
	return errors.Join(collect(errc)...)
}

func resources(in map[string]string) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	for path, value := range in {
		out[strings.Trim(path, "/")] = json.RawMessage(value)
	}
	return out
}

func collect(errc <-chan error) []error {
	var out []error
	for err := range errc {
		out = append(out, err)
	}
	return out
}

func runClient(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("client", flag.ExitOnError)
	addr := fs.String("addr", "", "gateway host:port (port 5684)")
	code := fs.String("code", os.Getenv("OVERGANG_SECURITY_CODE"), "security code, needed only the first time")
	identity := fs.String("identity", "overgang-client", "identity to pair as")
	creds := fs.String("creds", "overgang-client.json", "where to keep paired keys")
	verbose := fs.Bool("v", false, "debug logging")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: overgang client -addr host:5684 [flags] dump | get PATH | put PATH JSON | observe PATH")
		fs.PrintDefaults()
	}
	fs.Parse(args)
	setupLog(*verbose)
	if *addr == "" || fs.NArg() == 0 {
		fs.Usage()
		os.Exit(2)
	}

	store, err := state.Open(*creds)
	if err != nil {
		return err
	}
	key := *addr + "|" + *identity
	psk, ok := store.PSK(key)
	if !ok {
		if *code == "" {
			return errors.New("not paired yet: pass -code with the gateway's security code")
		}
		if psk, err = client.Pair(ctx, *addr, *code, *identity); err != nil {
			return err
		}
		if err := store.SetPSK(key, psk); err != nil {
			return err
		}
		slog.Info("paired", "identity", *identity)
	}
	c, err := client.Dial(ctx, *addr, *identity, psk)
	if err != nil {
		return err
	}
	defer c.Close()

	switch cmd := fs.Arg(0); {
	case cmd == "dump":
		out, err := c.Dump(ctx)
		if out != nil {
			b, _ := json.MarshalIndent(out, "", "  ")
			fmt.Println(string(b))
		}
		return err
	case cmd == "get" && fs.NArg() == 2:
		b, err := c.Get(ctx, strings.Trim(fs.Arg(1), "/"))
		if err != nil {
			return err
		}
		fmt.Println(string(b))
	case cmd == "put" && fs.NArg() == 3:
		return c.Put(ctx, strings.Trim(fs.Arg(1), "/"), []byte(fs.Arg(2)))
	case cmd == "observe" && fs.NArg() == 2:
		return c.Observe(ctx, strings.Trim(fs.Arg(1), "/"), func(b []byte) { fmt.Println(string(b)) })
	default:
		fs.Usage()
		os.Exit(2)
	}
	return nil
}

func runProxy(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("proxy", flag.ExitOnError)
	listen := fs.String("listen", ":5684", "UDP address the app connects to")
	gw := fs.String("gateway", "", "real gateway host:port")
	code := fs.String("code", os.Getenv("OVERGANG_SECURITY_CODE"), "the real gateway's security code")
	statePath := fs.String("state", "overgang-proxy.json", "where to keep identities learned from pairing")
	capture := fs.String("capture", "", "also append every message to this JSON-lines file")
	announce := fs.String("mdns-id", "", "announce over mDNS as gw-<this>, so the app can find the proxy")
	verbose := fs.Bool("v", false, "debug logging")
	fs.Parse(args)
	setupLog(*verbose)
	if *gw == "" || *code == "" {
		return errors.New("proxy needs -gateway and -code")
	}

	store, err := state.Open(*statePath)
	if err != nil {
		return err
	}
	p := &proxy.Proxy{Listen: *listen, Gateway: *gw, SecurityCode: *code, Store: store}
	if *capture != "" {
		f, err := os.OpenFile(*capture, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		defer f.Close()
		var mu sync.Mutex
		enc := json.NewEncoder(f)
		p.Capture = func(r proxy.Record) {
			mu.Lock()
			defer mu.Unlock()
			_ = enc.Encode(r)
		}
	}
	if *announce != "" {
		_, port, _ := net.SplitHostPort(*listen)
		n, _ := strconv.Atoi(port)
		go func() {
			if err := mdns.Announce(ctx, *announce, n, "proxy"); err != nil {
				slog.Error("mdns", "err", err)
			}
		}()
	}
	return p.Run(ctx)
}
