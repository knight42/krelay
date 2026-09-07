package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/tailscale/tailcat"
	"tailscale.com/tailcfg"
)

func TestDERPRegionResolverLabel(t *testing.T) {
	derpMap := `{
		"Regions": {
			"2": {
				"RegionID": 2,
				"RegionCode": "sfo",
				"RegionName": "San Francisco",
				"Nodes": [{"Name": "2a", "RegionID": 2, "HostName": "derp2.example.com"}]
			}
		}
	}`
	srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, derpMap)
	}))
	// The in-memory server's URL is only set once Client is first called.
	client := srv.Client()
	mapPath := filepath.Join(t.TempDir(), "derpmap.json")
	if err := os.WriteFile(mapPath, []byte(derpMap), 0o600); err != nil {
		t.Fatal(err)
	}

	embedded := (&tailcat.ConnInfo{Region: []*tailcfg.DERPRegion{{
		RegionID:   2,
		RegionCode: "sfo",
		Nodes:      []*tailcfg.DERPNode{{Name: "2a", RegionID: 2, HostName: "derp2.example.com"}},
	}}}).Addr()
	idOnly := (&tailcat.ConnInfo{RegionID: 2}).Addr()

	tests := map[string]struct {
		url  string
		addr tailcat.Addr
		want string
	}{
		"embedded region via URL map":  {url: srv.URL, addr: embedded, want: "sfo(2)"},
		"embedded region via file map": {url: "file://" + mapPath, addr: embedded, want: "sfo(2)"},
		"region ID only":               {url: srv.URL, addr: idOnly, want: "sfo(2)"},
		"unreadable map falls back":    {url: "file://" + filepath.Join(t.TempDir(), "missing.json"), addr: embedded, want: "derp2.example.com"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			r := startDERPRegionResolver(context.Background(), client, tt.url)
			if got := r.label(context.Background(), tt.addr); got != tt.want {
				t.Errorf("label() = %q, want %q", got, tt.want)
			}
		})
	}
}
