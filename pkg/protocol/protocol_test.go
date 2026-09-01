package protocol

import (
	"bytes"
	"errors"
	"testing"
)

func TestDialRequestRoundTrip(t *testing.T) {
	testCases := map[string]struct {
		target string
	}{
		"host and port": {target: "redis.example.com:6379"},
		"ip and port":   {target: "10.0.0.1:80"},
		"heartbeat":     {target: ""},
	}
	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := WriteDialRequest(&buf, tc.target); err != nil {
				t.Fatalf("write: %v", err)
			}
			got, err := ReadDialRequest(&buf)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if got != tc.target {
				t.Fatalf("got %q, want %q", got, tc.target)
			}
		})
	}
}

func TestReadDialRequestBadMagic(t *testing.T) {
	_, err := ReadDialRequest(bytes.NewReader([]byte("GET / HTTP/1.1\r\n")))
	if err == nil {
		t.Fatal("expected error for bad magic")
	}
}

func TestDialResponseRoundTrip(t *testing.T) {
	testCases := map[string]struct {
		dialErr error
		wantMsg string
	}{
		"success": {dialErr: nil},
		"failure": {dialErr: errors.New("connection refused"), wantMsg: "connection refused"},
	}
	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := WriteDialResponse(&buf, tc.dialErr); err != nil {
				t.Fatalf("write: %v", err)
			}
			err := ReadDialResponse(&buf)
			if tc.dialErr == nil {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || err.Error() != tc.wantMsg {
				t.Fatalf("got %v, want message %q", err, tc.wantMsg)
			}
		})
	}
}
