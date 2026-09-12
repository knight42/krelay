package main

import "testing"

func TestSOCKSListenAddress(t *testing.T) {
	for name, tt := range map[string]struct {
		address string
		args    []string
		want    string
	}{
		"default":       {address: "127.0.0.1", want: "127.0.0.1:1080"},
		"custom":        {address: "127.0.0.1", args: []string{"1081"}, want: "127.0.0.1:1081"},
		"ephemeral":     {address: "127.0.0.1", args: []string{"0"}, want: "127.0.0.1:0"},
		"ipv6":          {address: "::1", args: []string{"1080"}, want: "[::1]:1080"},
		"wildcard":      {address: "0.0.0.0", want: "0.0.0.0:1080"},
		"negative":      {address: "127.0.0.1", args: []string{"-1"}},
		"overflow":      {address: "127.0.0.1", args: []string{"65536"}},
		"service name":  {address: "127.0.0.1", args: []string{"http"}},
		"command":       {address: "127.0.0.1", args: []string{"1080", "curl"}},
		"empty port":    {address: "127.0.0.1", args: []string{""}},
		"hostname bind": {address: "localhost"},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := socksListenAddress(tt.address, tt.args)
			if tt.want == "" {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("got (%q, %v), want %q", got, err, tt.want)
			}
		})
	}
}
