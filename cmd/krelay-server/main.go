// Command krelay-server runs inside the cluster. It starts a tailcat server,
// prints its connection token to stdout for the client to read from pod logs,
// and relays every tunnel connection to the destination named in its dial
// request.
package main

import (
	"bytes"
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

	// The pre-shared key is mixed into every WireGuard handshake; the server
	// and the address handed to the client must carry the same one.
	psk := tailcat.NewPresharedKey()
	ci := &tailcat.ConnInfo{
		ServerPublic:      tailcat.NodePublic{NodePublic: priv.Public()},
		ServerDiscoPublic: tailcat.DiscoPublicForNode(priv),
		PresharedKey:      psk,
		Region:            []*tailcfg.DERPRegion{region},
	}
	token := ci.Addr()

	tracker := &activityTracker{}
	tracker.lastActivity.Store(time.Now().UnixNano())

	srv := &tailcat.Server{
		Key:            priv,
		PresharedKey:   psk,
		Region:         region,
		AllowedClients: []key.NodePublic{clientKey},
		ServedTCPPorts: []filter.PortRange{{First: constants.TunnelPort, Last: constants.TunnelPort}},
		ServedUDPPorts: []filter.PortRange{{First: constants.TunnelPort, Last: constants.TunnelPort}},
		OnTCP: func(port uint16) func(net.Conn) {
			if port != constants.TunnelPort {
				return nil // RST
			}
			return func(c net.Conn) { handleTunnelConn(c, tracker) }
		},
		OnUDP: func(port uint16) func(tailcat.ConnPacketConn) {
			if port != constants.TunnelPort {
				return nil
			}
			return func(c tailcat.ConnPacketConn) { handleUDPFlow(c, tracker) }
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

// handleUDPFlow relays one UDP flow. The first datagram carries a dial
// request naming the destination; the server acknowledges it with a dial
// response datagram and then relays datagrams verbatim in both directions.
// tailcat closes the flow after its UDPIdleTimeout of inactivity.
func handleUDPFlow(c tailcat.ConnPacketConn, tracker *activityTracker) {
	tracker.connStarted()
	defer tracker.connEnded()
	defer c.Close()

	buf := make([]byte, 65535)
	_ = c.SetReadDeadline(time.Now().Add(30 * time.Second))
	n, err := c.Read(buf)
	if err != nil {
		return
	}
	target, err := protocol.ReadDialRequest(bytes.NewReader(buf[:n]))
	if err != nil || target == "" {
		log.Printf("bad UDP dial request: %v", err)
		return
	}
	request := bytes.Clone(buf[:n])

	upstream, err := net.Dial("udp", target)
	var ack bytes.Buffer
	_ = protocol.WriteDialResponse(&ack, err)
	if _, werr := c.Write(ack.Bytes()); werr != nil || err != nil {
		if err != nil {
			log.Printf("dial udp %s: %v", target, err)
		}
		return
	}
	defer upstream.Close()
	_ = c.SetReadDeadline(time.Time{})
	log.Printf("connected to %s (udp)", target)

	// The upstream peer cannot send anything before the first datagram we
	// forward reveals our source address, so nothing is lost by pumping
	// upstream->client from the start.
	go func() {
		b := make([]byte, 65535)
		for {
			n, err := upstream.Read(b)
			if err != nil {
				return
			}
			if _, err := c.Write(b[:n]); err != nil {
				return
			}
		}
	}()
	awaitingFirst := true
	for {
		n, err := c.Read(buf)
		if err != nil {
			return
		}
		if awaitingFirst && bytes.Equal(buf[:n], request) {
			// The client re-sent the dial request, meaning our ack was
			// lost: acknowledge again. Once real data has flowed the
			// client is past the handshake and never re-sends.
			if _, err := c.Write(ack.Bytes()); err != nil {
				return
			}
			continue
		}
		awaitingFirst = false
		if _, err := upstream.Write(buf[:n]); err != nil {
			return
		}
	}
}
