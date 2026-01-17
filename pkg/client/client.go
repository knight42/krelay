// Package client provides the krelay client functionality.
package client

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/knight/krelay/pkg/apis/relay/v1alpha1"
)

// Client is a krelay client.
type Client struct {
	config *rest.Config
}

// NewClient creates a new krelay client from kubeconfig.
func NewClient(kubeconfig, context string) (*Client, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		loadingRules.ExplicitPath = kubeconfig
	}

	configOverrides := &clientcmd.ConfigOverrides{}
	if context != "" {
		configOverrides.CurrentContext = context
	}

	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		loadingRules,
		configOverrides,
	).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to load kubeconfig: %w", err)
	}

	return &Client{config: config}, nil
}

// GetCurrentNamespace returns the current namespace from kubeconfig.
func (c *Client) GetCurrentNamespace(kubeconfig, context string) (string, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		loadingRules.ExplicitPath = kubeconfig
	}

	configOverrides := &clientcmd.ConfigOverrides{}
	if context != "" {
		configOverrides.CurrentContext = context
	}

	clientConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		loadingRules,
		configOverrides,
	)

	ns, _, err := clientConfig.Namespace()
	if err != nil {
		return "default", nil
	}
	if ns == "" {
		return "default", nil
	}
	return ns, nil
}

// TunnelOptions contains options for creating a tunnel.
type TunnelOptions struct {
	Target    string
	Namespace string
	Port      int
	Protocol  v1alpha1.Protocol
}

// Tunnel creates a WebSocket tunnel to the target.
func (c *Client) Tunnel(ctx context.Context, opts TunnelOptions) (*websocket.Conn, error) {
	// Build the tunnel URL
	tunnelURL, err := c.buildTunnelURL(opts)
	if err != nil {
		return nil, err
	}

	// Create HTTP client with auth from kubeconfig
	dialer, err := c.createWebSocketDialer()
	if err != nil {
		return nil, err
	}

	// Add auth headers
	headers := http.Header{}
	if c.config.BearerToken != "" {
		headers.Set("Authorization", "Bearer "+c.config.BearerToken)
	}

	conn, resp, err := dialer.DialContext(ctx, tunnelURL, headers)
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("failed to connect to tunnel (status %d): %w", resp.StatusCode, err)
		}
		return nil, fmt.Errorf("failed to connect to tunnel: %w", err)
	}

	return conn, nil
}

func (c *Client) buildTunnelURL(opts TunnelOptions) (string, error) {
	// The tunnel endpoint goes through the K8s API server to the aggregated API
	// URL format: wss://<api-server>/apis/relay.krelay.io/v1alpha1/tunnel?...

	host := c.config.Host
	if !strings.HasPrefix(host, "https://") && !strings.HasPrefix(host, "http://") {
		host = "https://" + host
	}

	// Convert to WebSocket URL
	wsURL := strings.Replace(host, "https://", "wss://", 1)
	wsURL = strings.Replace(wsURL, "http://", "ws://", 1)

	u, err := url.Parse(wsURL)
	if err != nil {
		return "", fmt.Errorf("invalid host URL: %w", err)
	}

	u.Path = "/apis/relay.krelay.io/v1alpha1/tunnel"

	q := u.Query()
	q.Set("target", opts.Target)
	if opts.Namespace != "" {
		q.Set("namespace", opts.Namespace)
	}
	q.Set("port", fmt.Sprintf("%d", opts.Port))
	q.Set("protocol", string(opts.Protocol))
	u.RawQuery = q.Encode()

	return u.String(), nil
}

func (c *Client) createWebSocketDialer() (*websocket.Dialer, error) {
	// Use rest.TLSConfigFor to properly handle all TLS settings from kubeconfig
	tlsConfig, err := rest.TLSConfigFor(c.config)
	if err != nil {
		return nil, fmt.Errorf("failed to create TLS config: %w", err)
	}

	// If no TLS config could be created, create a basic one
	if tlsConfig == nil {
		tlsConfig = &tls.Config{
			MinVersion: tls.VersionTLS12,
		}
		if c.config.Insecure {
			tlsConfig.InsecureSkipVerify = true
		}
	}

	dialer := &websocket.Dialer{
		TLSClientConfig:  tlsConfig,
		HandshakeTimeout: 30 * time.Second,
		NetDialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			d := &net.Dialer{
				Timeout: 30 * time.Second,
			}
			return d.DialContext(ctx, network, addr)
		},
	}

	return dialer, nil
}
