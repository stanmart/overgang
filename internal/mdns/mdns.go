// Package mdns announces the gateway the way the real one does: a _coap._udp service
// named gw-<mac>, which is how the app finds gateways on the LAN.
package mdns

import (
	"context"
	"log/slog"

	"github.com/libp2p/zeroconf/v2"
)

// Announce advertises the service until ctx is done.
func Announce(ctx context.Context, gatewayID string, port int, firmware string) error {
	name := "gw-" + gatewayID
	srv, err := zeroconf.Register(name, "_coap._udp", "local.", port, []string{"version=" + firmware}, nil)
	if err != nil {
		return err
	}
	slog.Info("announcing over mDNS", "name", name, "port", port)
	<-ctx.Done()
	srv.Shutdown()
	return nil
}
