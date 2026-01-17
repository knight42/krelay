package server

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	"github.com/knight/krelay/pkg/apis/relay/v1alpha1"
	"github.com/knight/krelay/pkg/tunnel"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  32 * 1024,
	WriteBufferSize: 32 * 1024,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

// Handler handles tunnel requests.
type Handler struct {
	resolver *Resolver
}

// NewHandler creates a new Handler.
func NewHandler(client kubernetes.Interface) *Handler {
	return &Handler{
		resolver: NewResolver(client),
	}
}

// ServeHTTP handles HTTP requests.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/apis/relay.krelay.io/v1alpha1/tunnel":
		h.handleTunnel(w, r)
	case "/apis/relay.krelay.io/v1alpha1":
		h.handleDiscovery(w, r)
	case "/healthz", "/readyz", "/livez":
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	default:
		http.NotFound(w, r)
	}
}

func (h *Handler) handleDiscovery(w http.ResponseWriter, r *http.Request) {
	// Return API group version info for aggregated API server
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{
		"kind": "APIResourceList",
		"apiVersion": "v1",
		"groupVersion": "relay.krelay.io/v1alpha1",
		"resources": []
	}`))
}

func (h *Handler) handleTunnel(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Parse query parameters
	req, err := parseRequest(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	klog.V(2).Infof("Tunnel request: %+v", req)

	// Resolve target
	target, err := h.resolver.Resolve(ctx, req)
	if err != nil {
		klog.Errorf("Failed to resolve target: %v", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer target.StopWatch()

	// Upgrade to WebSocket
	wsConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		klog.Errorf("Failed to upgrade to WebSocket: %v", err)
		return
	}
	defer wsConn.Close()

	klog.V(2).Infof("WebSocket connection established, connecting to %s", target.Address)

	// Handle based on protocol
	switch req.Protocol {
	case v1alpha1.ProtocolTCP:
		h.handleTCP(ctx, wsConn, target, req)
	case v1alpha1.ProtocolUDP:
		h.handleUDP(ctx, wsConn, target, req)
	}
}

func (h *Handler) handleTCP(ctx context.Context, wsConn *websocket.Conn, target *ResolvedTarget, req *v1alpha1.TunnelRequest) {
	tun := tunnel.New(wsConn, v1alpha1.ProtocolTCP)
	dead := tun.StartPing(ctx)

	currentAddr := target.Address
	var conn net.Conn
	var err error

	// Connect to initial target
	conn, err = net.DialTimeout("tcp", currentAddr, 10*time.Second)
	if err != nil {
		klog.Errorf("Failed to connect to %s: %v", currentAddr, err)
		return
	}

	// For deployment targets with auto-reconnect
	if target.Watcher != nil {
		go func() {
			for newAddr := range target.Watcher {
				klog.V(2).Infof("Pod changed, reconnecting to %s", newAddr)
				newConn, err := net.DialTimeout("tcp", newAddr, 10*time.Second)
				if err != nil {
					klog.Errorf("Failed to reconnect to %s: %v", newAddr, err)
					continue
				}
				oldConn := conn
				conn = newConn
				currentAddr = newAddr
				oldConn.Close()
			}
		}()
	}

	// Proxy traffic
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	go func() {
		select {
		case <-dead:
			cancel()
		case <-ctx.Done():
		}
	}()

	if err := proxyTCP(ctx, wsConn, conn); err != nil && ctx.Err() == nil {
		klog.V(2).Infof("TCP proxy ended: %v", err)
	}

	conn.Close()
}

func (h *Handler) handleUDP(ctx context.Context, wsConn *websocket.Conn, target *ResolvedTarget, req *v1alpha1.TunnelRequest) {
	tun := tunnel.New(wsConn, v1alpha1.ProtocolUDP)
	dead := tun.StartPing(ctx)

	// Resolve UDP address
	udpAddr, err := net.ResolveUDPAddr("udp", target.Address)
	if err != nil {
		klog.Errorf("Failed to resolve UDP address %s: %v", target.Address, err)
		return
	}

	// Create UDP connection
	conn, err := net.DialUDP("udp", nil, udpAddr)
	if err != nil {
		klog.Errorf("Failed to dial UDP %s: %v", target.Address, err)
		return
	}
	defer conn.Close()

	// Proxy traffic
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	go func() {
		select {
		case <-dead:
			cancel()
		case <-ctx.Done():
		}
	}()

	if err := proxyUDP(ctx, wsConn, conn); err != nil && ctx.Err() == nil {
		klog.V(2).Infof("UDP proxy ended: %v", err)
	}
}

func proxyTCP(ctx context.Context, wsConn *websocket.Conn, tcpConn net.Conn) error {
	var wg sync.WaitGroup
	errCh := make(chan error, 2)

	// WebSocket -> TCP
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			_, data, err := wsConn.ReadMessage()
			if err != nil {
				if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
					errCh <- nil
				} else {
					errCh <- err
				}
				return
			}
			if _, err := tcpConn.Write(data); err != nil {
				errCh <- err
				return
			}
		}
	}()

	// TCP -> WebSocket
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, 32*1024)
		for {
			n, err := tcpConn.Read(buf)
			if err != nil {
				if err == io.EOF {
					errCh <- nil
				} else {
					errCh <- err
				}
				return
			}
			if err := wsConn.WriteMessage(websocket.BinaryMessage, buf[:n]); err != nil {
				errCh <- err
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

func proxyUDP(ctx context.Context, wsConn *websocket.Conn, udpConn *net.UDPConn) error {
	errCh := make(chan error, 2)

	// WebSocket -> UDP
	go func() {
		for {
			_, data, err := wsConn.ReadMessage()
			if err != nil {
				if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
					errCh <- nil
				} else {
					errCh <- err
				}
				return
			}
			if _, err := udpConn.Write(data); err != nil {
				errCh <- err
				return
			}
		}
	}()

	// UDP -> WebSocket
	go func() {
		buf := make([]byte, 65535)
		for {
			udpConn.SetReadDeadline(time.Now().Add(30 * time.Second))
			n, err := udpConn.Read(buf)
			if err != nil {
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					continue // timeout is ok for UDP, just keep reading
				}
				errCh <- err
				return
			}
			if err := wsConn.WriteMessage(websocket.BinaryMessage, buf[:n]); err != nil {
				errCh <- err
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

func parseRequest(r *http.Request) (*v1alpha1.TunnelRequest, error) {
	q := r.URL.Query()

	target := q.Get("target")
	if target == "" {
		return nil, fmt.Errorf("missing 'target' parameter")
	}

	targetType, targetName, err := v1alpha1.ParseTarget(target)
	if err != nil {
		return nil, err
	}

	namespace := q.Get("namespace")
	if namespace == "" && (targetType == v1alpha1.TargetTypePod || targetType == v1alpha1.TargetTypeService || targetType == v1alpha1.TargetTypeDeployment) {
		return nil, fmt.Errorf("missing 'namespace' parameter for %s target", targetType)
	}

	portStr := q.Get("port")
	if portStr == "" {
		return nil, fmt.Errorf("missing 'port' parameter")
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return nil, fmt.Errorf("invalid 'port' parameter: %s", portStr)
	}

	protocol := v1alpha1.Protocol(q.Get("protocol"))
	if protocol == "" {
		protocol = v1alpha1.ProtocolTCP
	}
	if protocol != v1alpha1.ProtocolTCP && protocol != v1alpha1.ProtocolUDP {
		return nil, fmt.Errorf("invalid 'protocol' parameter: %s", protocol)
	}

	return &v1alpha1.TunnelRequest{
		TargetType: targetType,
		TargetName: targetName,
		Namespace:  namespace,
		Port:       port,
		Protocol:   protocol,
	}, nil
}
