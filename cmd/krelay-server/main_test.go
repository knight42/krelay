package main

import (
	"testing"

	"tailscale.com/ipn/ipnstate"
)

func TestPeerPath(t *testing.T) {
	for name, tt := range map[string]struct {
		peer *ipnstate.PeerStatus
		want string
	}{
		"not connected": {},
		"path pending":  {peer: &ipnstate.PeerStatus{}},
		"relayed":       {peer: &ipnstate.PeerStatus{Relay: "sfo"}, want: "derp sfo"},
		"direct with relay available": {
			peer: &ipnstate.PeerStatus{CurAddr: "192.0.2.1:1234", Relay: "sfo"},
			want: "direct 192.0.2.1:1234",
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got := peerPath(tt.peer); got != tt.want {
				t.Fatalf("peerPath() = %q, want %q", got, tt.want)
			}
		})
	}
}
