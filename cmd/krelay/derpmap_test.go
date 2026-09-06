package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDerpMapArg(t *testing.T) {
	dir := t.TempDir()
	writeFile := func(name, content string) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	validMap := `{
		"Regions": {
			"900": {
				"RegionID": 900,
				"RegionName": "custom",
				"Nodes": [{"Name": "900a", "RegionID": 900, "HostName": "derp.example.com"}]
			}
		}
	}`
	validPath := writeFile("valid.json", validMap)

	testCases := map[string]struct {
		url     string
		want    string
		wantErr string
	}{
		"http URL passes through": {
			url:  "https://derp.example.com/derpmap.json",
			want: "--derp-map-url=https://derp.example.com/derpmap.json",
		},
		"file URL inlines compacted JSON": {
			url:  "file://" + validPath,
			want: `--derp-map-json={"Regions":{"900":{"RegionID":900,"RegionName":"custom","Nodes":[{"Name":"900a","RegionID":900,"HostName":"derp.example.com"}]}}}`,
		},
		"missing file": {
			url:     "file://" + filepath.Join(dir, "nope.json"),
			wantErr: "read DERP map",
		},
		"invalid JSON": {
			url:     "file://" + writeFile("bad.json", "{not json"),
			wantErr: "invalid DERP map JSON",
		},
		"wrong schema": {
			url:     "file://" + writeFile("schema.json", `{"Regions": "oops"}`),
			wantErr: "invalid DERP map JSON",
		},
		"no regions": {
			url:     "file://" + writeFile("empty.json", `{"Regions": {}}`),
			wantErr: "has no regions",
		},
	}
	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			got, err := derpMapArg(tc.url)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("got error %v, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
}
