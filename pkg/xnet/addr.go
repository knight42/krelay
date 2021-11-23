package xnet

import (
	"fmt"
	"net"
)

const (
	AddrTypeIPv4 byte = iota
	AddrTypeIPv6
	AddrTypeHost
)

type Addr struct {
	Type byte
	IP   net.IP
	Host string
}

func (a *Addr) Marshal() []byte {
	switch {
	case a.IP != nil:
		return a.IP
	default:
		return []byte(a.Host)
	}
}

func (a *Addr) String() string {
	switch {
	case a.IP != nil:
		return a.IP.String()
	default:
		return a.Host
	}
}

func AddrFromBytes(addrType byte, data []byte) Addr {
	switch addrType {
	case AddrTypeIPv4, AddrTypeIPv6:
		return Addr{Type: addrType, IP: data}
	default:
		return Addr{Type: addrType, Host: string(data)}
	}
}

func AddrFromIP(ipStr string) (Addr, error) {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return Addr{}, fmt.Errorf("invalid ip: %s", ipStr)
	}
	addrType := AddrTypeIPv4
	if len(ip) == net.IPv6len {
		addrType = AddrTypeIPv6
	}
	return Addr{Type: addrType, IP: ip}, nil
}
