package main

import (
	"testing"
)

func TestMuxID(t *testing.T) {
	testCases := map[string]struct {
		a, b     []string
		wantSame bool
	}{
		"same inputs match": {
			a:        []string{"ctx", "ns", "node"},
			b:        []string{"ctx", "ns", "node"},
			wantSame: true,
		},
		"different node differs": {
			a: []string{"ctx", "ns", "node-1"},
			b: []string{"ctx", "ns", "node-2"},
		},
		"parts are delimited": {
			a: []string{"ab", "c"},
			b: []string{"a", "bc"},
		},
	}
	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			idA, idB := muxID(tc.a...), muxID(tc.b...)
			if (idA == idB) != tc.wantSame {
				t.Fatalf("muxID(%q) = %s, muxID(%q) = %s, wantSame = %v", tc.a, idA, tc.b, idB, tc.wantSame)
			}
			// The id is embedded in a unix socket path, which has a tight
			// length limit (~104 bytes on macOS).
			if len(idA) != 16 {
				t.Fatalf("len(muxID) = %d, want 16", len(idA))
			}
		})
	}
}

func TestParseMuxStatus(t *testing.T) {
	testCases := map[string]struct {
		line       string
		wantStatus muxStatus
		wantMsg    string
		wantErr    bool
	}{
		"ready":              {line: "READY\n", wantStatus: muxReady},
		"busy":               {line: "BUSY\n", wantStatus: muxBusy},
		"error with message": {line: "ERROR node \"x\" not found\n", wantStatus: muxError, wantMsg: "node \"x\" not found"},
		"unknown":            {line: "BOGUS\n", wantErr: true},
		"empty":              {line: "", wantErr: true},
	}
	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			status, msg, err := parseMuxStatus(tc.line)
			if (err != nil) != tc.wantErr {
				t.Fatalf("parseMuxStatus(%q) error = %v, wantErr = %v", tc.line, err, tc.wantErr)
			}
			if err != nil {
				return
			}
			if status != tc.wantStatus || msg != tc.wantMsg {
				t.Fatalf("parseMuxStatus(%q) = (%v, %q), want (%v, %q)", tc.line, status, msg, tc.wantStatus, tc.wantMsg)
			}
		})
	}
}
