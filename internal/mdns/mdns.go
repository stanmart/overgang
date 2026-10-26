// Package mdns announces the gateway the way the real one does: a _coap._udp service
// named gw-<id> on a host named TRADFRI-Gateway-<id>, where <id> is the gateway's MAC
// without separators.
package mdns

import (
	"context"
	"log/slog"
	"net"

	"github.com/libp2p/zeroconf/v2"
)

// Announce advertises the service until ctx is done.
func Announce(ctx context.Context, gatewayID string, port int, firmware string) error {
	name := "gw-" + gatewayID
	host := "TRADFRI-Gateway-" + gatewayID
	ips := localIPv4s()
	srv, err := zeroconf.RegisterProxy(name, "_coap._udp", "local.", port, host, ips, []string{"version=" + firmware}, nil)
	if err != nil {
		return err
	}
	slog.Info("announcing over mDNS", "name", name, "host", host+".local", "ips", ips, "port", port)
	<-ctx.Done()
	srv.Shutdown()
	return nil
}

func localIPv4s() []string {
	var out []string
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLoopback() && !n.IP.IsLinkLocalUnicast() {
			out = append(out, n.IP.String())
		}
	}
	return out
}
