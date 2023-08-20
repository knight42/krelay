package xnet

import (
	"bytes"
	"net"
	"testing"

	"github.com/stretchr/testify/require"
)

var fakeRequestID = "00000"

var headerCases = map[string]struct {
	hdr   Header
	bytes []byte
}{
	"host": {
		hdr: Header{
			Version:   1,
			RequestID: fakeRequestID,
			Protocol:  ProtocolTCP,
			Port:      80,
			Addr:      AddrFromHost("a.com"),
		},
		bytes: []byte{
			1,
			0, 0x11,
			0x30, 0x30, 0x30, 0x30, 0x30,
			0,
			0, 80,
			1,
			97, 46, 99, 111, 109,
		},
	},
	"ipv4": {
		hdr: Header{
			Version:   0,
			RequestID: fakeRequestID,
			Protocol:  ProtocolUDP,
			Port:      53,
			Addr:      AddrFromBytes(AddrTypeIP, net.IPv4(192, 168, 1, 1).To4()),
		},
		bytes: []byte{
			0,
			0, 0x10,
			0x30, 0x30, 0x30, 0x30, 0x30,
			1,
			0, 53,
			0,
			192, 168, 1, 1,
		},
	},
	"ipv6": {
		hdr: Header{
			Version:   0,
			RequestID: fakeRequestID,
			Protocol:  ProtocolTCP,
			Port:      8080,
			Addr:      AddrFromBytes(AddrTypeIP, net.IPv6loopback),
		},
		bytes: []byte{
			0,
			0, 0x1c,
			0x30, 0x30, 0x30, 0x30, 0x30,
			0,
			0x1f, 0x90,
			0,
			0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1,
		},
	},
}

func TestHeaderMarshal(t *testing.T) {
	for name, tc := range headerCases {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.bytes, tc.hdr.Marshal())
		})
	}
}

func TestHeaderUnmarshal(t *testing.T) {
	for name, tc := range headerCases {
		t.Run(name, func(t *testing.T) {
			r := require.New(t)
			got := Header{}
			r.NoError(got.FromReader(bytes.NewBuffer(tc.bytes)))
			r.Equal(tc.hdr, got)
		})
	}
}
