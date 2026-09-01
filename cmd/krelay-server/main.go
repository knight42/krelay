// Command krelay-server runs inside the cluster. It starts a tailcat server,
// prints its connection token to stdout for the client to read from pod logs,
// and relays every tunnel connection to the destination named in its dial
// request.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/tailscale/tailcat"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
	"tailscale.com/wgengine/filter"

	"github.com/knight42/krelay/pkg/constants"
	"github.com/knight42/krelay/pkg/protocol"
)

type activityTracker struct {
	activeConns  atomic.Int64
	lastActivity atomic.Int64 // unix nano
}

func (t *activityTracker) connStarted() {
	t.lastActivity.Store(time.Now().UnixNano())
	t.activeConns.Add(1)
}

func (t *activityTracker) connEnded() {
	// Refresh lastActivity before decrementing so the idle monitor never
	// observes zero connections alongside a stale timestamp.
	t.lastActivity.Store(time.Now().UnixNano())
	t.activeConns.Add(-1)
}

func (t *activityTracker) idleFor() (time.Duration, bool) {
	if t.activeConns.Load() > 0 {
		return 0, false
	}
	return time.Since(time.Unix(0, t.lastActivity.Load())), true
}

func main() {
	var (
		allowedClient string
		derpMapURL    string
		idleTimeout   time.Duration
	)
	flag.StringVar(&allowedClient, "allowed-client", "", "Node public key of the only client allowed to connect (required).")
	flag.StringVar(&derpMapURL, "derp-map-url", tailcat.DefaultDERPMapURL, "URL of the DERP map used to pick the bootstrap relay.")
	flag.DurationVar(&idleTimeout, "idle-timeout", constants.DefaultIdleTimeout, "Exit after this long with no active tunnel connections.")
	flag.Parse()

	var clientKey key.NodePublic
	if err := clientKey.UnmarshalText([]byte(allowedClient)); err != nil {
		log.Fatalf("invalid --allowed-client %q: %v", allowedClient, err)
	}

	priv := key.NewNode()

	// Pick the nearest DERP region and embed its full details in the token,
	// so the client never needs to fetch the DERP map. This keeps self-hosted
	// DERP setups to a single --derp-map-url flag on the client.
	pick := &tailcat.ConnInfo{RegionID: -1}
	if err := pick.Expand(context.Background(), tailcat.ExpandForServer, tailcat.DERPMapURL(derpMapURL)); err != nil {
		log.Fatalf("pick DERP region: %v", err)
	}
	region := pick.Region[0]
	log.Printf("selected DERP region %d (%s)", region.RegionID, region.RegionName)

	ci := &tailcat.ConnInfo{
		ServerPublic:      tailcat.NodePublic{NodePublic: priv.Public()},
		ServerDiscoPublic: tailcat.DiscoPublicForNode(priv),
		Region:            []*tailcfg.DERPRegion{region},
	}
	token := ci.ConnBlob()

	tracker := &activityTracker{}
	tracker.lastActivity.Store(time.Now().UnixNano())

	srv := &tailcat.Server{
		Key:            priv,
		Region:         region,
		AllowedClients: []key.NodePublic{clientKey},
		ServedTCPPorts: []filter.PortRange{{First: constants.TunnelPort, Last: constants.TunnelPort}},
		OnTCP: func(port uint16) func(net.Conn) {
			if port != constants.TunnelPort {
				return nil // RST
			}
			return func(c net.Conn) { handleTunnelConn(c, tracker) }
		},
	}
	if err := srv.Start(); err != nil {
		log.Fatalf("start tailcat server: %v", err)
	}

	// The client watches pod logs for this line.
	fmt.Printf("%s%s\n", constants.TokenPrefix, token)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case sig := <-sigCh:
			log.Printf("received %v, exiting", sig)
			return
		case <-ticker.C:
			if idle, ok := tracker.idleFor(); ok && idle > idleTimeout {
				log.Printf("no active connections for %v, exiting", idle.Round(time.Second))
				return
			}
		}
	}
}

func handleTunnelConn(c net.Conn, tracker *activityTracker) {
	tracker.connStarted()
	defer tracker.connEnded()
	defer c.Close()

	target, err := protocol.ReadDialRequest(c)
	if err != nil {
		log.Printf("read dial request: %v", err)
		return
	}

	if target == "" {
		// Heartbeat connection: acknowledge and hold it open. Its presence
		// keeps activeConns above zero so the idle monitor stays quiet.
		if err := protocol.WriteDialResponse(c, nil); err != nil {
			return
		}
		buf := make([]byte, 1)
		for {
			if _, err := c.Read(buf); err != nil {
				return
			}
		}
	}

	upstream, err := net.DialTimeout("tcp", target, 10*time.Second)
	if err != nil {
		log.Printf("dial %s: %v", target, err)
		_ = protocol.WriteDialResponse(c, err)
		return
	}
	if err := protocol.WriteDialResponse(c, nil); err != nil {
		upstream.Close()
		return
	}
	log.Printf("connected to %s", target)
	tailcat.ProxyConns(c, upstream)
}
