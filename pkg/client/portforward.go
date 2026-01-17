package client

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"

	"github.com/gorilla/websocket"

	"github.com/knight/krelay/pkg/apis/relay/v1alpha1"
	"github.com/knight/krelay/pkg/tunnel"
)

// PortForwarder handles port forwarding.
type PortForwarder struct {
	client    *Client
	target    string
	namespace string
	mappings  []v1alpha1.PortMapping
	listeners []net.Listener
	udpConns  []*net.UDPConn
	mu        sync.Mutex
}

// NewPortForwarder creates a new PortForwarder.
func NewPortForwarder(client *Client, target, namespace string, mappings []v1alpha1.PortMapping) *PortForwarder {
	return &PortForwarder{
		client:    client,
		target:    target,
		namespace: namespace,
		mappings:  mappings,
	}
}

// Run starts the port forwarding.
func (pf *PortForwarder) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	errCh := make(chan error, len(pf.mappings))
	var wg sync.WaitGroup

	for _, mapping := range pf.mappings {
		wg.Add(1)
		go func(m v1alpha1.PortMapping) {
			defer wg.Done()
			if err := pf.forwardPort(ctx, m); err != nil && ctx.Err() == nil {
				errCh <- fmt.Errorf("port %d: %w", m.LocalPort, err)
			}
		}(mapping)
	}

	// Wait for first error or context cancellation
	select {
	case err := <-errCh:
		cancel()
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (pf *PortForwarder) forwardPort(ctx context.Context, mapping v1alpha1.PortMapping) error {
	switch mapping.Protocol {
	case v1alpha1.ProtocolTCP:
		return pf.forwardTCP(ctx, mapping)
	case v1alpha1.ProtocolUDP:
		return pf.forwardUDP(ctx, mapping)
	default:
		return fmt.Errorf("unsupported protocol: %s", mapping.Protocol)
	}
}

func (pf *PortForwarder) forwardTCP(ctx context.Context, mapping v1alpha1.PortMapping) error {
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", mapping.LocalPort))
	if err != nil {
		return fmt.Errorf("failed to listen on port %d: %w", mapping.LocalPort, err)
	}
	defer listener.Close()

	pf.mu.Lock()
	pf.listeners = append(pf.listeners, listener)
	pf.mu.Unlock()

	fmt.Printf("Forwarding TCP 127.0.0.1:%d -> %s:%d\n", mapping.LocalPort, pf.target, mapping.RemotePort)

	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
				return fmt.Errorf("accept error: %w", err)
			}
		}

		go pf.handleTCPConnection(ctx, conn, mapping)
	}
}

func (pf *PortForwarder) handleTCPConnection(ctx context.Context, conn net.Conn, mapping v1alpha1.PortMapping) {
	defer conn.Close()

	// Create tunnel to remote
	wsConn, err := pf.client.Tunnel(ctx, TunnelOptions{
		Target:    pf.target,
		Namespace: pf.namespace,
		Port:      mapping.RemotePort,
		Protocol:  v1alpha1.ProtocolTCP,
	})
	if err != nil {
		fmt.Printf("Failed to create tunnel: %v\n", err)
		return
	}
	defer wsConn.Close()

	tun := tunnel.New(wsConn, v1alpha1.ProtocolTCP)
	dead := tun.StartPing(ctx)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	go func() {
		select {
		case <-dead:
			cancel()
		case <-ctx.Done():
		}
	}()

	// Proxy traffic
	var wg sync.WaitGroup
	wg.Add(2)

	// Local -> WebSocket
	go func() {
		defer wg.Done()
		buf := make([]byte, 32*1024)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				if err != io.EOF {
					// Connection closed
				}
				return
			}
			if err := tun.WriteMessage(buf[:n]); err != nil {
				return
			}
		}
	}()

	// WebSocket -> Local
	go func() {
		defer wg.Done()
		for {
			data, err := tun.ReadMessage()
			if err != nil {
				return
			}
			if _, err := conn.Write(data); err != nil {
				return
			}
		}
	}()

	wg.Wait()
}

func (pf *PortForwarder) forwardUDP(ctx context.Context, mapping v1alpha1.PortMapping) error {
	addr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("127.0.0.1:%d", mapping.LocalPort))
	if err != nil {
		return fmt.Errorf("failed to resolve UDP address: %w", err)
	}

	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return fmt.Errorf("failed to listen on UDP port %d: %w", mapping.LocalPort, err)
	}
	defer conn.Close()

	pf.mu.Lock()
	pf.udpConns = append(pf.udpConns, conn)
	pf.mu.Unlock()

	fmt.Printf("Forwarding UDP 127.0.0.1:%d -> %s:%d\n", mapping.LocalPort, pf.target, mapping.RemotePort)

	// Track client sessions (remote addr -> websocket tunnel)
	sessions := &sync.Map{}

	buf := make([]byte, 65535)
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		n, remoteAddr, err := conn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
				continue
			}
		}

		// Get or create session for this client
		sessionKey := remoteAddr.String()
		session, loaded := sessions.LoadOrStore(sessionKey, &udpSession{})
		s := session.(*udpSession)

		if !loaded {
			// New session, create tunnel
			wsConn, err := pf.client.Tunnel(ctx, TunnelOptions{
				Target:    pf.target,
				Namespace: pf.namespace,
				Port:      mapping.RemotePort,
				Protocol:  v1alpha1.ProtocolUDP,
			})
			if err != nil {
				sessions.Delete(sessionKey)
				fmt.Printf("Failed to create UDP tunnel: %v\n", err)
				continue
			}

			s.wsConn = wsConn
			s.localConn = conn
			s.remoteAddr = remoteAddr

			// Start reading from WebSocket and sending back to client
			go func() {
				defer func() {
					sessions.Delete(sessionKey)
					wsConn.Close()
				}()

				for {
					_, data, err := wsConn.ReadMessage()
					if err != nil {
						return
					}
					if _, err := conn.WriteToUDP(data, remoteAddr); err != nil {
						return
					}
				}
			}()
		}

		// Forward packet to remote
		if s.wsConn != nil {
			if err := s.wsConn.WriteMessage(websocket.BinaryMessage, buf[:n]); err != nil {
				sessions.Delete(sessionKey)
				s.wsConn.Close()
			}
		}
	}
}

type udpSession struct {
	wsConn     *websocket.Conn
	localConn  *net.UDPConn
	remoteAddr *net.UDPAddr
}

// Close closes all listeners.
func (pf *PortForwarder) Close() {
	pf.mu.Lock()
	defer pf.mu.Unlock()

	for _, l := range pf.listeners {
		l.Close()
	}
	for _, c := range pf.udpConns {
		c.Close()
	}
}
