// Command krelay is a kubectl plugin (kubectl-relay) that forwards local TCP
// ports to targets reachable from inside a Kubernetes cluster. It launches a
// krelay-server Job in the cluster and exchanges traffic with it over a
// tailcat (WireGuard + DERP) tunnel, bypassing the apiserver for data.
package main

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/tailscale/tailcat"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"

	"github.com/knight42/krelay/pkg/kube"
	"github.com/knight42/krelay/pkg/ports"
	"github.com/knight42/krelay/pkg/resolver"
)

var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

type options struct {
	cf *genericclioptions.ConfigFlags

	address          string
	targetsFile      string
	serverImage      string
	serverPullPolicy string
	serverNamespace  string
	derpMapURL       string
	// serverToken, if set, skips creating the server Job and connects to an
	// already-running krelay-server. Intended for development and testing.
	serverToken string
	verbosity   int
}

// derpMapArg returns the krelay-server flag conveying the DERP map choice.
// A file:// URL names a file on this machine, which the server pod cannot
// read, so its contents are validated and passed inline instead of the URL.
func derpMapArg(derpMapURL string) (string, error) {
	path, ok := strings.CutPrefix(derpMapURL, "file://")
	if !ok {
		return "--derp-map-url=" + derpMapURL, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read DERP map %s: %w", derpMapURL, err)
	}
	// Validate here: a bad map would otherwise only surface as a server
	// pod crash after the Job is created.
	var dm tailcfg.DERPMap
	if err := json.Unmarshal(data, &dm); err != nil {
		return "", fmt.Errorf("invalid DERP map JSON in %s: %w", path, err)
	}
	if len(dm.Regions) == 0 {
		return "", fmt.Errorf("DERP map %s has no regions", path)
	}
	compact := jsontext.Value(data)
	if err := compact.Compact(); err != nil {
		return "", fmt.Errorf("invalid DERP map JSON in %s: %w", path, err)
	}
	return "--derp-map-json=" + string(compact), nil
}

// startServer creates the krelay-server Job and returns the ServerJob handle.
// When nodeName is non-empty, the pod is scheduled on that node with hostPID
// and privileged access for SSH mode. The caller must Close it when done.
func startServer(ctx context.Context, o *options, priv key.NodePrivate, nodeName string) (*kube.ServerJob, error) {
	restCfg, err := o.cf.ToRESTConfig()
	if err != nil {
		return nil, err
	}
	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, err
	}
	derpArg, err := derpMapArg(o.derpMapURL)
	if err != nil {
		return nil, err
	}
	args := []string{
		"--allowed-client=" + priv.Public().String(),
		derpArg,
	}
	if nodeName != "" {
		args = append(args, "--ssh")
	}
	return kube.RunServerJob(ctx, cs, kube.ServerOptions{
		Namespace:  o.serverNamespace,
		Image:      o.serverImage,
		PullPolicy: o.serverPullPolicy,
		Args:       args,
		NodeName:   nodeName,
	})
}

func (o *options) run(ctx context.Context, args []string) error {
	namespace, _, err := o.cf.ToRawKubeConfigLoader().Namespace()
	if err != nil {
		return fmt.Errorf("get namespace: %w", err)
	}

	// SSH mode: `kubectl relay ssh/NODE`
	if len(args) >= 1 {
		_, nodeName, isSSH := parseSSHTarget(args[0])
		if isSSH {
			// Verify the node exists.
			restCfg, err := o.cf.ToRESTConfig()
			if err != nil {
				return err
			}
			cs, err := kubernetes.NewForConfig(restCfg)
			if err != nil {
				return err
			}
			if _, err := cs.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{}); err != nil {
				return fmt.Errorf("node %q: %w", nodeName, err)
			}
			var localPort string
			if len(args) >= 2 {
				localPort = args[1]
			}
			return o.runSSH(ctx, nodeName, localPort)
		}
	}

	var specs []targetSpec
	if o.targetsFile != "" {
		if len(args) != 0 {
			return errors.New("a targets file and TYPE/NAME with ports cannot be specified at the same time")
		}
		specs, err = parseTargetsFile(o.targetsFile, namespace, o.address)
		if err != nil {
			return err
		}
	} else {
		if len(args) < 2 {
			return errors.New("TYPE/NAME and a list of ports are required")
		}
		specs = []targetSpec{{
			resource:   args[0],
			ports:      args[1:],
			namespace:  namespace,
			listenAddr: o.address,
		}}
	}

	restCfg, err := o.cf.ToRESTConfig()
	if err != nil {
		return err
	}
	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return err
	}

	var fwds []*forwarder
	for _, spec := range specs {
		t, err := resolver.ParseTarget(spec.resource, spec.namespace)
		if err != nil {
			return err
		}
		getter, namedPorts, err := resolver.Resolve(ctx, cs, t)
		if err != nil {
			return fmt.Errorf("resolve %s: %w", spec.resource, err)
		}
		pairs, err := ports.Parse(spec.ports, namedPorts)
		if err != nil {
			return fmt.Errorf("parse ports of %s: %w", spec.resource, err)
		}
		for _, pair := range pairs {
			fwds = append(fwds, &forwarder{
				target:     t,
				getter:     getter,
				ports:      pair,
				listenAddr: spec.listenAddr,
			})
		}
	}

	bound := 0
	for _, f := range fwds {
		if err := f.listen(); err != nil {
			slog.Error("Fail to listen", slog.String("address", f.listenAddr), slog.Any("port", f.ports.Local), slog.Any("error", err))
			continue
		}
		defer f.close()
		bound++
	}
	if bound == 0 {
		return errors.New("unable to listen on any of the requested ports")
	}
	for _, f := range fwds {
		if f.bound() && f.ports.Proto == ports.ProtocolUDP {
			// Measured behavior, not just documentation: oversized datagrams
			// are dropped silently on both the direct and the DERP path.
			slog.Warn("UDP datagrams larger than the tunnel MTU are dropped",
				slog.Int("maxPayload", tailcat.MaxUDPPayload))
			break
		}
	}

	// Start loading the DERP map now so the region code is usually ready,
	// at no extra latency, by the time the tunnel logs mention the region.
	regions := startDERPRegionResolver(ctx, http.DefaultClient, o.derpMapURL)

	priv := key.NewNode()
	token := o.serverToken
	if token == "" {
		sj, err := startServer(ctx, o, priv, "")
		if err != nil {
			return err
		}
		defer sj.Close()

		tokenCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		token, err = sj.ReadToken(tokenCtx)
		cancel()
		if err != nil {
			return fmt.Errorf("read connection token from pod logs: %w", err)
		}
		slog.Debug("Got connection token", slog.String("token", token))
	}

	tcLogf := logger.Discard
	if o.verbosity >= 5 {
		tcLogf = logger.WithPrefix(log.Printf, "tailcat: ")
	}
	tc := &tailcat.Client{
		Server: tailcat.Addr(token),
		Key:    priv,
		Logf:   tcLogf,
	}
	defer tc.Close()

	slog.Info("Establishing tunnel to krelay-server")
	establishCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	err = establishTunnel(establishCtx, tc, regions)
	cancel()
	if err != nil {
		return fmt.Errorf("establish tunnel: %w", err)
	}

	go maintainHeartbeat(ctx, tc)
	go monitorPath(ctx, tc, regions)
	for _, f := range fwds {
		if f.bound() {
			go f.run(ctx, tc)
		}
	}

	<-ctx.Done()
	return nil
}

func main() {
	o := options{
		cf: genericclioptions.NewConfigFlags(true),
	}
	printVersion := false

	c := cobra.Command{
		Use: fmt.Sprintf("%s TYPE/NAME [options] [LOCAL_PORT:]REMOTE_PORT [...[LOCAL_PORT_N:]REMOTE_PORT_N]", programName()),
		Long: `Forward local TCP/UDP ports to a pod, service, workload, IP or hostname
reachable from inside the cluster, or SSH into a cluster node.

Traffic flows over an end-to-end encrypted tailcat (WireGuard) tunnel
between this machine and a short-lived krelay-server pod, instead of
through the Kubernetes apiserver.

SSH mode (ssh/NODE [LOCAL_PORT]):
  Creates a privileged pod on the target node and uses nsenter to give
  you a root shell in the host namespaces — like kubectl node-shell,
  but over WireGuard. Prints a local address you can ssh into.`,
		Example: example(),
		Args:    cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if printVersion {
				err := json.MarshalWrite(cmd.OutOrStdout(), struct {
					Version   string
					Commit    string
					BuildDate string
				}{version, commit, date})
				if err != nil {
					return err
				}
				_, err = fmt.Fprintln(cmd.OutOrStdout())
				return err
			}
			slog.SetLogLoggerLevel(logLevel(o.verbosity))
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
			defer cancel()
			return o.run(ctx, args)
		},
		SilenceUsage: true,
	}

	flags := c.Flags()
	flags.SortFlags = false
	flags.StringVar(o.cf.KubeConfig, "kubeconfig", *o.cf.KubeConfig, "Path to the kubeconfig file to use for CLI requests.")
	flags.StringVarP(o.cf.Namespace, "namespace", "n", *o.cf.Namespace, "If present, the namespace scope for this CLI request.")
	flags.StringVar(o.cf.Context, "context", *o.cf.Context, "The name of the kubeconfig context to use.")
	flags.StringVar(o.cf.ClusterName, "cluster", *o.cf.ClusterName, "The name of the kubeconfig cluster to use.")
	flags.BoolVarP(&printVersion, "version", "V", false, "Print version info and exit.")
	flags.StringVarP(&o.address, "address", "l", "127.0.0.1", "Address to listen on. Only accepts IP addresses as a value.")
	flags.StringVarP(&o.targetsFile, "file", "f", "", "Forward to the targets specified in the given file, with one target per line. \"-\" reads from stdin.")
	flags.StringVar(&o.serverImage, "server.image", "ghcr.io/knight42/krelay-server:v2", "The krelay-server image to use.")
	flags.StringVar(&o.serverPullPolicy, "server.pull-policy", "IfNotPresent", "Image pull policy of the krelay-server pod.")
	flags.StringVar(&o.serverNamespace, "server.namespace", "default", "Namespace to create the krelay-server Job in.")
	flags.StringVar(&o.derpMapURL, "derp-map-url", tailcat.DefaultDERPMapURL, "URL of the DERP map used to bootstrap the tunnel. Point this at your own DERP deployment to avoid third-party relays. A file:// URL is read locally and its contents are sent to the server pod.")
	flags.StringVar(&o.serverToken, "server-token", "", "Connect to an existing krelay-server using this token instead of creating one.")
	_ = flags.MarkHidden("server-token")
	flags.IntVarP(&o.verbosity, "v", "v", 3, "Number for the log level verbosity. The bigger the more verbose.")

	if c.Execute() != nil {
		os.Exit(1)
	}
}

func programName() string {
	name := filepath.Base(os.Args[0])
	if name == "kubectl-relay" {
		return "kubectl relay"
	}
	return name
}

func example() string {
	name := programName()
	return fmt.Sprintf(`  # Forward local port 8080 to port 80 of service "nginx"
  %[1]s svc/nginx 8080:80

  # Forward local port 6379 to a hostname resolved inside the cluster
  %[1]s host/redis.cn-north-1.cache.amazonaws.com 6379

  # Forward local port 5000 to port 5000 of deployment "backend",
  # surviving rolling updates
  %[1]s deploy/backend 5000

  # Forward local port 5353 to port 53 of the IP 10.96.0.10
  %[1]s ip/10.96.0.10 5353:53

  # SSH into a cluster node (prints local address to ssh into)
  %[1]s ssh/my-node-01

  # SSH, listening on a specific local port
  %[1]s ssh/my-node-01 2222

  # Forward multiple targets defined in a file
  %[1]s -f targets.txt`, name)
}

func logLevel(verbosity int) slog.Level {
	switch {
	case verbosity >= 4:
		return slog.LevelDebug
	case verbosity == 3:
		return slog.LevelInfo
	case verbosity == 2:
		return slog.LevelWarn
	default:
		return slog.LevelError
	}
}
