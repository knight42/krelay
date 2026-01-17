package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/knight/krelay/pkg/apis/relay/v1alpha1"
	"github.com/knight/krelay/pkg/client"
)

var (
	kubeconfig string
	kubecontext string
	namespace  string
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "krelay",
		Short: "Forward traffic to Kubernetes cluster",
		Long:  "krelay is a command-line utility that forwards TCP and UDP traffic to applications running inside a Kubernetes cluster.",
	}

	rootCmd.PersistentFlags().StringVar(&kubeconfig, "kubeconfig", "", "Path to kubeconfig file")
	rootCmd.PersistentFlags().StringVar(&kubecontext, "context", "", "Kubernetes context to use")

	rootCmd.AddCommand(newPortForwardCmd())
	rootCmd.AddCommand(newProxyCmd())

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func newPortForwardCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "port-forward <target> <port>...",
		Short: "Forward local ports to a target in the cluster",
		Long: `Forward local ports to a target in the cluster.

Target types:
  pod/<name>     - Forward to a specific pod (session ends if pod dies)
  svc/<name>     - Forward via service (survives rolling updates)
  deploy/<name>  - Forward to deployment pod (auto-reconnects if pod dies)
  ip/<address>   - Forward to arbitrary IP inside cluster
  host/<hostname>- Forward to hostname resolved inside cluster

Port format:
  [local_port:]<remote_port>[@protocol]

Examples:
  krelay port-forward pod/nginx 8080:80
  krelay port-forward svc/api 8080:80 8443:443
  krelay port-forward -n production deploy/web 8080:80
  krelay port-forward svc/dns 5353:53@udp
  krelay port-forward ip/10.96.0.1 443
  krelay port-forward host/kubernetes.default.svc 443`,
		Args: cobra.MinimumNArgs(2),
		RunE: runPortForward,
	}

	cmd.Flags().StringVarP(&namespace, "namespace", "n", "", "Target namespace (defaults to current context's namespace)")

	return cmd
}

func runPortForward(cmd *cobra.Command, args []string) error {
	target := args[0]
	portArgs := args[1:]

	// Parse target
	targetType, _, err := v1alpha1.ParseTarget(target)
	if err != nil {
		return err
	}

	// Parse port mappings
	var mappings []v1alpha1.PortMapping
	for _, p := range portArgs {
		mapping, err := v1alpha1.ParsePortMapping(p)
		if err != nil {
			return fmt.Errorf("invalid port mapping %q: %w", p, err)
		}
		mappings = append(mappings, mapping)
	}

	// Create client
	c, err := client.NewClient(kubeconfig, kubecontext)
	if err != nil {
		return err
	}

	// Determine namespace
	ns := namespace
	if ns == "" && (targetType == v1alpha1.TargetTypePod || targetType == v1alpha1.TargetTypeService || targetType == v1alpha1.TargetTypeDeployment) {
		ns, err = c.GetCurrentNamespace(kubeconfig, kubecontext)
		if err != nil {
			return err
		}
	}

	// Create port forwarder
	pf := client.NewPortForwarder(c, target, ns, mappings)
	defer pf.Close()

	// Handle signals
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigCh
		fmt.Println("\nShutting down...")
		cancel()
	}()

	// Run port forwarding
	return pf.Run(ctx)
}

func newProxyCmd() *cobra.Command {
	var protocol string

	cmd := &cobra.Command{
		Use:   "proxy <listen_addr>",
		Short: "Run a proxy server",
		Long: `Run a proxy server that routes traffic through the cluster.

Supported protocols:
  http   - HTTP proxy (default)
  socks5 - SOCKS5 proxy

Examples:
  krelay proxy :8080
  krelay proxy -p socks5 :1080
  krelay proxy --protocol=http 127.0.0.1:8080`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runProxy(args[0], protocol)
		},
	}

	cmd.Flags().StringVarP(&protocol, "protocol", "p", "http", "Proxy protocol (http or socks5)")

	return cmd
}

func runProxy(addr, protocol string) error {
	// Create client
	c, err := client.NewClient(kubeconfig, kubecontext)
	if err != nil {
		return err
	}

	// Handle signals
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigCh
		fmt.Println("\nShutting down...")
		cancel()
	}()

	switch protocol {
	case "http":
		return runHTTPProxy(ctx, c, addr)
	case "socks5":
		return runSOCKS5Proxy(ctx, c, addr)
	default:
		return fmt.Errorf("unsupported protocol: %s (expected http or socks5)", protocol)
	}
}

func runHTTPProxy(ctx context.Context, c *client.Client, addr string) error {
	proxy := client.NewHTTPProxy(c)
	return proxy.ListenAndServe(ctx, addr)
}

func runSOCKS5Proxy(ctx context.Context, c *client.Client, addr string) error {
	proxy := client.NewSOCKS5Proxy(c)
	return proxy.ListenAndServe(ctx, addr)
}
