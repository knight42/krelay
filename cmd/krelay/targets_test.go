package main

import (
	"reflect"
	"testing"
)

func TestParseTargetLine(t *testing.T) {
	testCases := map[string]struct {
		line    string
		want    targetSpec
		wantErr bool
	}{
		"simple": {
			line: "svc/nginx 8080:80",
			want: targetSpec{resource: "svc/nginx", ports: []string{"8080:80"}, namespace: "default", listenAddr: "127.0.0.1"},
		},
		"multiple ports": {
			line: "deploy/backend 5000 6000:6060",
			want: targetSpec{resource: "deploy/backend", ports: []string{"5000", "6000:6060"}, namespace: "default", listenAddr: "127.0.0.1"},
		},
		"namespace override": {
			line: "-n kube-system svc/kube-dns 10053:53",
			want: targetSpec{resource: "svc/kube-dns", ports: []string{"10053:53"}, namespace: "kube-system", listenAddr: "127.0.0.1"},
		},
		"listen address override": {
			line: "-l 192.168.1.101 host/redis.example.com 6379",
			want: targetSpec{resource: "host/redis.example.com", ports: []string{"6379"}, namespace: "default", listenAddr: "192.168.1.101"},
		},
		"both overrides": {
			line: "-n prod -l 0.0.0.0 svc/api 8080:80",
			want: targetSpec{resource: "svc/api", ports: []string{"8080:80"}, namespace: "prod", listenAddr: "0.0.0.0"},
		},
		"missing ports": {
			line:    "svc/nginx",
			wantErr: true,
		},
		"missing namespace value": {
			line:    "svc/nginx 80 -n",
			wantErr: true,
		},
	}
	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			got, err := parseTargetLine(tc.line, "default", "127.0.0.1")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}
