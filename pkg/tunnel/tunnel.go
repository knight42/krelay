// Package tunnel provides WebSocket-based tunneling for TCP and UDP traffic.
package tunnel

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/knight/krelay/pkg/apis/relay/v1alpha1"
)

const (
	// PingInterval is how often to send ping frames.
	PingInterval = 30 * time.Second

	// PongTimeout is how long to wait for a pong response.
	PongTimeout = 60 * time.Second

	// WriteTimeout is the timeout for write operations.
	WriteTimeout = 10 * time.Second
)

// Tunnel represents a WebSocket tunnel for TCP or UDP traffic.
type Tunnel struct {
	conn     *websocket.Conn
	protocol v1alpha1.Protocol
	mu       sync.Mutex
}

// New creates a new tunnel from a WebSocket connection.
func New(conn *websocket.Conn, protocol v1alpha1.Protocol) *Tunnel {
	return &Tunnel{
		conn:     conn,
		protocol: protocol,
	}
}

// StartPing starts sending periodic ping frames.
// Returns a channel that is closed when the connection is dead.
func (t *Tunnel) StartPing(ctx context.Context) <-chan struct{} {
	dead := make(chan struct{})

	// Set up pong handler
	lastPong := time.Now()
	var pongMu sync.Mutex
	t.conn.SetPongHandler(func(string) error {
		pongMu.Lock()
		lastPong = time.Now()
		pongMu.Unlock()
		return nil
	})

	go func() {
		ticker := time.NewTicker(PingInterval)
		defer ticker.Stop()
		defer close(dead)

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				t.mu.Lock()
				err := t.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(WriteTimeout))
				t.mu.Unlock()
				if err != nil {
					return
				}

				// Check if we've received a pong recently
				pongMu.Lock()
				since := time.Since(lastPong)
				pongMu.Unlock()
				if since > PongTimeout {
					return
				}
			}
		}
	}()

	return dead
}

// ProxyTCP proxies TCP traffic between the WebSocket and a net.Conn.
func (t *Tunnel) ProxyTCP(ctx context.Context, conn net.Conn) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	errCh := make(chan error, 2)

	// WebSocket -> TCP
	go func() {
		for {
			_, data, err := t.conn.ReadMessage()
			if err != nil {
				if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
					errCh <- nil
				} else {
					errCh <- fmt.Errorf("websocket read: %w", err)
				}
				return
			}
			if _, err := conn.Write(data); err != nil {
				errCh <- fmt.Errorf("tcp write: %w", err)
				return
			}
		}
	}()

	// TCP -> WebSocket
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				if err == io.EOF {
					errCh <- nil
				} else {
					errCh <- fmt.Errorf("tcp read: %w", err)
				}
				return
			}

			t.mu.Lock()
			err = t.conn.WriteMessage(websocket.BinaryMessage, buf[:n])
			t.mu.Unlock()
			if err != nil {
				errCh <- fmt.Errorf("websocket write: %w", err)
				return
			}
		}
	}()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-errCh:
		return err
	}
}

// ProxyUDP proxies UDP traffic between the WebSocket and a UDP connection.
// For UDP, each WebSocket message is one datagram.
func (t *Tunnel) ProxyUDP(ctx context.Context, conn *net.UDPConn, remoteAddr *net.UDPAddr) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	errCh := make(chan error, 2)

	// WebSocket -> UDP
	go func() {
		for {
			_, data, err := t.conn.ReadMessage()
			if err != nil {
				if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
					errCh <- nil
				} else {
					errCh <- fmt.Errorf("websocket read: %w", err)
				}
				return
			}
			if _, err := conn.WriteToUDP(data, remoteAddr); err != nil {
				errCh <- fmt.Errorf("udp write: %w", err)
				return
			}
		}
	}()

	// UDP -> WebSocket
	go func() {
		buf := make([]byte, 65535) // max UDP datagram size
		for {
			n, addr, err := conn.ReadFromUDP(buf)
			if err != nil {
				errCh <- fmt.Errorf("udp read: %w", err)
				return
			}
			// Only forward packets from expected remote
			if remoteAddr != nil && !addr.IP.Equal(remoteAddr.IP) {
				continue
			}

			t.mu.Lock()
			err = t.conn.WriteMessage(websocket.BinaryMessage, buf[:n])
			t.mu.Unlock()
			if err != nil {
				errCh <- fmt.Errorf("websocket write: %w", err)
				return
			}
		}
	}()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-errCh:
		return err
	}
}

// Close closes the tunnel.
func (t *Tunnel) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.conn.Close()
}

// WriteMessage writes a message to the WebSocket.
func (t *Tunnel) WriteMessage(data []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.conn.WriteMessage(websocket.BinaryMessage, data)
}

// ReadMessage reads a message from the WebSocket.
func (t *Tunnel) ReadMessage() ([]byte, error) {
	_, data, err := t.conn.ReadMessage()
	return data, err
}
