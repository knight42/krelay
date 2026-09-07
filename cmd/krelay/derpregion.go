package main

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/tailscale/tailcat"
	"tailscale.com/tailcfg"
)

// derpRegionResolver reverse-maps a relay hostname to its real DERP region.
// Tailcat addresses strip the region code to stay compact, so the only way to
// recover it is the DERP map the server picked its region from. Loading the
// map runs in the background, overlapping the much slower pod startup, and is
// best-effort: lookups fall back to the label embedded in the address.
type derpRegionResolver struct {
	done   chan struct{}
	byHost map[string]*tailcfg.DERPRegion
	byID   map[tailcfg.DERPRegionID]*tailcfg.DERPRegion
}

// startDERPRegionResolver starts loading the DERP map at derpMapURL.
func startDERPRegionResolver(ctx context.Context, client *http.Client, derpMapURL string) *derpRegionResolver {
	r := &derpRegionResolver{done: make(chan struct{})}
	go func() {
		defer close(r.done)
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		dm, err := loadDERPMap(ctx, client, derpMapURL)
		if err != nil {
			slog.Debug("Fail to load DERP map for region codes", slog.Any("error", err))
			return
		}
		r.byHost = make(map[string]*tailcfg.DERPRegion)
		r.byID = make(map[tailcfg.DERPRegionID]*tailcfg.DERPRegion)
		for _, region := range dm.Regions {
			r.byID[region.RegionID] = region
			for _, n := range region.Nodes {
				r.byHost[n.HostName] = region
			}
		}
	}()
	return r
}

// label returns a label for the DERP region addr points at, preferring the
// real region code from the DERP map. It waits briefly for the background
// load; on timeout or lookup miss it falls back to [addrDERPRegion].
func (r *derpRegionResolver) label(ctx context.Context, addr tailcat.Addr) string {
	select {
	case <-r.done:
	case <-ctx.Done():
		return addrDERPRegion(addr)
	case <-time.After(2 * time.Second):
		return addrDERPRegion(addr)
	}
	ci, err := tailcat.ParseAddr(addr)
	if err == nil {
		var region *tailcfg.DERPRegion
		if len(ci.Region) > 0 {
			for _, n := range ci.Region[0].Nodes {
				if region = r.byHost[n.HostName]; region != nil {
					break
				}
			}
		} else {
			region = r.byID[ci.RegionID]
		}
		if region != nil {
			if l := derpRegionLabel(region.RegionCode, region.RegionID); l != "" {
				return l
			}
		}
	}
	return addrDERPRegion(addr)
}

// loadDERPMap reads a file:// DERP map locally and fetches any other URL with
// client. The fetch is hand-rolled because tailcat.FetchDERPMap hard-codes
// http.DefaultClient.
func loadDERPMap(ctx context.Context, client *http.Client, derpMapURL string) (*tailcfg.DERPMap, error) {
	if path, ok := strings.CutPrefix(derpMapURL, "file://"); ok {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		dm := new(tailcfg.DERPMap)
		if err := json.Unmarshal(data, dm); err != nil {
			return nil, fmt.Errorf("invalid DERP map JSON in %s: %w", path, err)
		}
		return dm, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, derpMapURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch DERP map %s: %s", derpMapURL, resp.Status)
	}
	dm := new(tailcfg.DERPMap)
	if err := json.UnmarshalRead(resp.Body, dm); err != nil {
		return nil, fmt.Errorf("invalid DERP map JSON from %s: %w", derpMapURL, err)
	}
	return dm, nil
}
