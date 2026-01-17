package client

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"

	"github.com/knight/krelay/pkg/apis/relay/v1alpha1"
	"github.com/knight/krelay/pkg/tunnel"
)

// SOCKS5 constants
const (
	socks5Version = 0x05

	// Auth methods
	socks5AuthNone     = 0x00
	socks5AuthPassword = 0x02
	socks5AuthNoAccept = 0xFF

	// Commands
	socks5CmdConnect = 0x01

	// Address types
	socks5AddrIPv4   = 0x01
	socks5AddrDomain = 0x03
	socks5AddrIPv6   = 0x04

	// Reply codes
	socks5ReplySuccess         = 0x00
	socks5ReplyGeneralFailure  = 0x01
	socks5ReplyConnRefused     = 0x05
	socks5ReplyHostUnreachable = 0x04
)

// SOCKS5Proxy is a SOCKS5 proxy that routes traffic through the cluster.
type SOCKS5Proxy struct {
	client *Client
}

// NewSOCKS5Proxy creates a new SOCKS5 proxy.
func NewSOCKS5Proxy(client *Client) *SOCKS5Proxy {
	return &SOCKS5Proxy{client: client}
}

// ListenAndServe starts the SOCKS5 proxy server.
func (p *SOCKS5Proxy) ListenAndServe(ctx context.Context, addr string) error {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", addr, err)
	}
	defer listener.Close()

	fmt.Printf("SOCKS5 proxy listening on %s\n", addr)

	go func() {
		<-ctx.Done()
		listener.Close()
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
				continue
			}
		}

		go p.handleConnection(ctx, conn)
	}
}

func (p *SOCKS5Proxy) handleConnection(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	// Handshake
	if err := p.handshake(conn); err != nil {
		return
	}

	// Read request
	host, port, err := p.readRequest(conn)
	if err != nil {
		p.sendReply(conn, socks5ReplyGeneralFailure, nil)
		return
	}

	// Create tunnel through cluster
	wsConn, err := p.client.Tunnel(ctx, TunnelOptions{
		Target:    "host/" + host,
		Namespace: "",
		Port:      port,
		Protocol:  v1alpha1.ProtocolTCP,
	})
	if err != nil {
		p.sendReply(conn, socks5ReplyHostUnreachable, nil)
		return
	}
	defer wsConn.Close()

	// Send success reply
	boundAddr := &net.TCPAddr{IP: net.IPv4zero, Port: 0}
	p.sendReply(conn, socks5ReplySuccess, boundAddr)

	// Start proxying
	tun := tunnel.New(wsConn, v1alpha1.ProtocolTCP)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	dead := tun.StartPing(ctx)
	go func() {
		select {
		case <-dead:
			cancel()
		case <-ctx.Done():
		}
	}()

	// Bidirectional copy
	errCh := make(chan error, 2)

	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				errCh <- err
				return
			}
			if err := tun.WriteMessage(buf[:n]); err != nil {
				errCh <- err
				return
			}
		}
	}()

	go func() {
		for {
			data, err := tun.ReadMessage()
			if err != nil {
				errCh <- err
				return
			}
			if _, err := conn.Write(data); err != nil {
				errCh <- err
				return
			}
		}
	}()

	select {
	case <-ctx.Done():
	case <-errCh:
	}
}

func (p *SOCKS5Proxy) handshake(conn net.Conn) error {
	// Read version and number of auth methods
	header := make([]byte, 2)
	if _, err := io.ReadFull(conn, header); err != nil {
		return err
	}

	if header[0] != socks5Version {
		return fmt.Errorf("unsupported SOCKS version: %d", header[0])
	}

	numMethods := int(header[1])
	methods := make([]byte, numMethods)
	if _, err := io.ReadFull(conn, methods); err != nil {
		return err
	}

	// We only support no auth
	hasNoAuth := false
	for _, m := range methods {
		if m == socks5AuthNone {
			hasNoAuth = true
			break
		}
	}

	if !hasNoAuth {
		conn.Write([]byte{socks5Version, socks5AuthNoAccept})
		return fmt.Errorf("no acceptable auth method")
	}

	// Send auth method selection
	conn.Write([]byte{socks5Version, socks5AuthNone})
	return nil
}

func (p *SOCKS5Proxy) readRequest(conn net.Conn) (string, int, error) {
	// Read request header: VER CMD RSV ATYP
	header := make([]byte, 4)
	if _, err := io.ReadFull(conn, header); err != nil {
		return "", 0, err
	}

	if header[0] != socks5Version {
		return "", 0, fmt.Errorf("unsupported SOCKS version: %d", header[0])
	}

	if header[1] != socks5CmdConnect {
		return "", 0, fmt.Errorf("unsupported command: %d", header[1])
	}

	// Read address based on type
	var host string
	switch header[3] {
	case socks5AddrIPv4:
		addr := make([]byte, 4)
		if _, err := io.ReadFull(conn, addr); err != nil {
			return "", 0, err
		}
		host = net.IP(addr).String()

	case socks5AddrDomain:
		lenByte := make([]byte, 1)
		if _, err := io.ReadFull(conn, lenByte); err != nil {
			return "", 0, err
		}
		domain := make([]byte, lenByte[0])
		if _, err := io.ReadFull(conn, domain); err != nil {
			return "", 0, err
		}
		host = string(domain)

	case socks5AddrIPv6:
		addr := make([]byte, 16)
		if _, err := io.ReadFull(conn, addr); err != nil {
			return "", 0, err
		}
		host = net.IP(addr).String()

	default:
		return "", 0, fmt.Errorf("unsupported address type: %d", header[3])
	}

	// Read port
	portBytes := make([]byte, 2)
	if _, err := io.ReadFull(conn, portBytes); err != nil {
		return "", 0, err
	}
	port := int(binary.BigEndian.Uint16(portBytes))

	return host, port, nil
}

func (p *SOCKS5Proxy) sendReply(conn net.Conn, reply byte, addr net.Addr) {
	var response []byte

	if addr == nil {
		// Send minimal reply with zero address
		response = []byte{
			socks5Version,
			reply,
			0x00, // RSV
			socks5AddrIPv4,
			0, 0, 0, 0, // IPv4 0.0.0.0
			0, 0, // Port 0
		}
	} else {
		tcpAddr, ok := addr.(*net.TCPAddr)
		if !ok {
			response = []byte{
				socks5Version,
				reply,
				0x00,
				socks5AddrIPv4,
				0, 0, 0, 0,
				0, 0,
			}
		} else {
			ip := tcpAddr.IP.To4()
			if ip == nil {
				ip = tcpAddr.IP.To16()
				response = make([]byte, 22)
				response[0] = socks5Version
				response[1] = reply
				response[2] = 0x00
				response[3] = socks5AddrIPv6
				copy(response[4:20], ip)
				binary.BigEndian.PutUint16(response[20:], uint16(tcpAddr.Port))
			} else {
				response = make([]byte, 10)
				response[0] = socks5Version
				response[1] = reply
				response[2] = 0x00
				response[3] = socks5AddrIPv4
				copy(response[4:8], ip)
				binary.BigEndian.PutUint16(response[8:], uint16(tcpAddr.Port))
			}
		}
	}

	conn.Write(response)
}

func init() {
	// Suppress unused import warning
	_ = strconv.Itoa
}
