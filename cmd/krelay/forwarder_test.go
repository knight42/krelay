package main

import (
	"testing"

	"github.com/tailscale/tailcat"
	"tailscale.com/tailcfg"
)

func TestDerpRegionLabel(t *testing.T) {
	tests := map[string]struct {
		code string
		id   tailcfg.DERPRegionID
		want string
	}{
		"real code":        {code: "sfo", id: 2, want: "sfo(2)"},
		"missing code":     {code: "", id: 2, want: ""},
		"synthesized code": {code: "2", id: 2, want: ""},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if got := derpRegionLabel(tt.code, tt.id); got != tt.want {
				t.Errorf("derpRegionLabel(%q, %d) = %q, want %q", tt.code, tt.id, got, tt.want)
			}
		})
	}
}

func TestAddrDERPRegion(t *testing.T) {
	tests := map[string]struct {
		ci   *tailcat.ConnInfo
		want string
	}{
		// The encoding strips the real region code, so the relay hostname
		// is the best label a parsed address can yield.
		"embedded region": {
			ci: &tailcat.ConnInfo{Region: []*tailcfg.DERPRegion{{
				RegionID:   900,
				RegionCode: "custom",
				Nodes:      []*tailcfg.DERPNode{{Name: "900a", RegionID: 900, HostName: "derp.example.com"}},
			}}},
			want: "derp.example.com",
		},
		"region ID only": {
			ci:   &tailcat.ConnInfo{RegionID: 2},
			want: "derp-2",
		},
		"auto region": {
			ci:   &tailcat.ConnInfo{RegionID: -1},
			want: "unknown",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if got := addrDERPRegion(tt.ci.Addr()); got != tt.want {
				t.Errorf("addrDERPRegion() = %q, want %q", got, tt.want)
			}
		})
	}
}
