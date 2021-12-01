package xnet

import (
	"net"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAddrSerialization(t *testing.T) {
	testCases := map[string]struct {
		getAddr       func(t *testing.T) Addr
		expectedBytes []byte
		expectedStr   string
	}{
		"AddrFromBytes: ipv4": {
			getAddr: func(t *testing.T) Addr {
				return AddrFromBytes(AddrTypeIP, net.IPv4(192, 168, 1, 1).To4())
			},
			expectedBytes: []byte{192, 168, 1, 1},
			expectedStr:   "192.168.1.1",
		},
		"AddrFromBytes: ipv6": {
			getAddr: func(t *testing.T) Addr {
				return AddrFromBytes(AddrTypeIP, net.IPv6linklocalallnodes)
			},
			expectedBytes: []byte{0xff, 0x02, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0x01},
			expectedStr:   "ff02::1",
		},
		"AddrFromHost": {
			getAddr: func(t *testing.T) Addr {
				return AddrFromHost("www.google.com")
			},
			expectedBytes: []byte("www.google.com"),
			expectedStr:   "www.google.com",
		},
		"AddrFromIP: ipv4": {
			getAddr: func(t *testing.T) Addr {
				addr, err := AddrFromIP("192.168.1.1")
				if err != nil {
					t.Fatal(err)
				}
				return addr
			},
			expectedBytes: []byte{192, 168, 1, 1},
			expectedStr:   "192.168.1.1",
		},
		"AddrFromIP: ipv6": {
			getAddr: func(t *testing.T) Addr {
				addr, err := AddrFromIP("ff02::1")
				if err != nil {
					t.Fatal(err)
				}
				return addr
			},
			expectedBytes: []byte{0xff, 0x02, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0x01},
			expectedStr:   "ff02::1",
		},
	}
	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			r := require.New(t)
			addr := tc.getAddr(t)
			r.Equal(tc.expectedBytes, addr.Marshal())
			r.Equal(tc.expectedStr, addr.String())
		})
	}
}
