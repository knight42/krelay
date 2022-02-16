package xnet

import (
	"fmt"
	"net"
)

const (
	AddrTypeIP byte = iota
	AddrTypeHost
)

type Addr struct {
	typ  byte
	data []byte
}

func (a *Addr) Marshal() []byte {
	return a.data
}

func (a *Addr) String() string {
	switch a.typ {
	case AddrTypeIP:
		return net.IP(a.data).String()
	default:
		return string(a.data)
	}
}

func (a *Addr) IsZero() bool {
	return len(a.data) == 0
}

func AddrFromBytes(addrType byte, data []byte) Addr {
	return Addr{typ: addrType, data: data}
}

func AddrFromIP(ipStr string) (Addr, error) {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return Addr{}, fmt.Errorf("invalid ip: %s", ipStr)
	}
	ipv4 := ip.To4()
	if ipv4 != nil {
		ip = ipv4
	}
	return Addr{typ: AddrTypeIP, data: ip}, nil
}

func AddrFromHost(host string) Addr {
	return Addr{typ: AddrTypeHost, data: []byte(host)}
}
