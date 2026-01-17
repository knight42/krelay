package client

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/knight/krelay/pkg/apis/relay/v1alpha1"
	"github.com/knight/krelay/pkg/tunnel"
)

// HTTPProxy is an HTTP proxy that routes traffic through the cluster.
type HTTPProxy struct {
	client *Client
}

// NewHTTPProxy creates a new HTTP proxy.
func NewHTTPProxy(client *Client) *HTTPProxy {
	return &HTTPProxy{client: client}
}

// ListenAndServe starts the HTTP proxy server.
func (p *HTTPProxy) ListenAndServe(ctx context.Context, addr string) error {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", addr, err)
	}
	defer listener.Close()

	fmt.Printf("HTTP proxy listening on %s\n", addr)

	server := &http.Server{
		Handler: p,
	}

	go func() {
		<-ctx.Done()
		server.Close()
	}()

	err = server.Serve(listener)
	if err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// ServeHTTP handles HTTP proxy requests.
func (p *HTTPProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.handleConnect(w, r)
	} else {
		p.handleHTTP(w, r)
	}
}

func (p *HTTPProxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	// CONNECT method for HTTPS tunneling
	host := r.Host
	if !strings.Contains(host, ":") {
		host = host + ":443"
	}

	hostname, port, err := net.SplitHostPort(host)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	portNum := 443
	fmt.Sscanf(port, "%d", &portNum)

	// Create tunnel through cluster
	wsConn, err := p.client.Tunnel(r.Context(), TunnelOptions{
		Target:    "host/" + hostname,
		Namespace: "",
		Port:      portNum,
		Protocol:  v1alpha1.ProtocolTCP,
	})
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to create tunnel: %v", err), http.StatusBadGateway)
		return
	}
	defer wsConn.Close()

	// Hijack the connection
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking not supported", http.StatusInternalServerError)
		return
	}

	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer clientConn.Close()

	// Send 200 Connection Established
	clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))

	// Start proxying
	tun := tunnel.New(wsConn, v1alpha1.ProtocolTCP)
	ctx, cancel := context.WithCancel(r.Context())
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
			n, err := clientConn.Read(buf)
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
			if _, err := clientConn.Write(data); err != nil {
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

func (p *HTTPProxy) handleHTTP(w http.ResponseWriter, r *http.Request) {
	// Regular HTTP proxy request
	if r.URL.Host == "" {
		http.Error(w, "missing host in request", http.StatusBadRequest)
		return
	}

	host := r.URL.Host
	if !strings.Contains(host, ":") {
		if r.URL.Scheme == "https" {
			host = host + ":443"
		} else {
			host = host + ":80"
		}
	}

	hostname, port, err := net.SplitHostPort(host)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	portNum := 80
	fmt.Sscanf(port, "%d", &portNum)

	// Create tunnel through cluster
	wsConn, err := p.client.Tunnel(r.Context(), TunnelOptions{
		Target:    "host/" + hostname,
		Namespace: "",
		Port:      portNum,
		Protocol:  v1alpha1.ProtocolTCP,
	})
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to create tunnel: %v", err), http.StatusBadGateway)
		return
	}
	defer wsConn.Close()

	// Hijack the connection for raw proxying
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking not supported", http.StatusInternalServerError)
		return
	}

	clientConn, clientBuf, err := hijacker.Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer clientConn.Close()

	tun := tunnel.New(wsConn, v1alpha1.ProtocolTCP)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	dead := tun.StartPing(ctx)
	go func() {
		select {
		case <-dead:
			cancel()
		case <-ctx.Done():
		}
	}()

	// Build and send the request to the remote server
	// Use HTTP/1.0 or add Connection: close to ensure server closes connection
	reqPath := r.URL.Path
	if r.URL.RawQuery != "" {
		reqPath += "?" + r.URL.RawQuery
	}
	if reqPath == "" {
		reqPath = "/"
	}

	// Build request
	var reqBuf strings.Builder
	fmt.Fprintf(&reqBuf, "%s %s HTTP/1.1\r\n", r.Method, reqPath)
	fmt.Fprintf(&reqBuf, "Host: %s\r\n", r.URL.Host)

	// Force connection close so server closes connection after response
	fmt.Fprintf(&reqBuf, "Connection: close\r\n")

	// Copy headers (skip hop-by-hop headers)
	for key, values := range r.Header {
		if hopHeaders[strings.ToLower(key)] {
			continue
		}
		if strings.ToLower(key) == "host" {
			continue // already added
		}
		for _, value := range values {
			fmt.Fprintf(&reqBuf, "%s: %s\r\n", key, value)
		}
	}
	fmt.Fprintf(&reqBuf, "\r\n")

	// Send headers through tunnel
	if err := tun.WriteMessage([]byte(reqBuf.String())); err != nil {
		return
	}

	// Send body if present
	if r.Body != nil && r.ContentLength != 0 {
		body, _ := io.ReadAll(r.Body)
		if len(body) > 0 {
			if err := tun.WriteMessage(body); err != nil {
				return
			}
		}
	}

	// Bidirectional copy
	errCh := make(chan error, 2)

	// Client -> Remote (for any buffered data or pipelined requests)
	go func() {
		if clientBuf.Reader.Buffered() > 0 {
			buffered := make([]byte, clientBuf.Reader.Buffered())
			clientBuf.Read(buffered)
			tun.WriteMessage(buffered)
		}
		buf := make([]byte, 32*1024)
		for {
			n, err := clientConn.Read(buf)
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

	// Remote -> Client
	go func() {
		for {
			data, err := tun.ReadMessage()
			if err != nil {
				errCh <- err
				return
			}
			if _, err := clientConn.Write(data); err != nil {
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

var hopHeaders = map[string]bool{
	"connection":          true,
	"keep-alive":          true,
	"proxy-authenticate":  true,
	"proxy-authorization": true,
	"proxy-connection":    true,
	"te":                  true,
	"trailer":             true,
	"transfer-encoding":   true,
	"upgrade":             true,
}

func init() {
	// Suppress unused import warning
	_ = time.Second
}
