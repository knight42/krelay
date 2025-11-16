package xnet

import (
	"fmt"
	"net"
	"strconv"
)

const (
	AddrTypeIP byte = iota
	AddrTypeHost
)

type AddrPort struct {
	addr Addr
	port uint16
}

func (a AddrPort) Port() uint16 {
	return a.port
}

func (a AddrPort) Addr() Addr {
	return a.addr
}

func (a AddrPort) String() string {
	host := a.addr.String()
	return net.JoinHostPort(host, strconv.Itoa(int(a.port)))
}

func AddrPortFrom(a Addr, port uint16) AddrPort {
	return AddrPort{a, port}
}

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
