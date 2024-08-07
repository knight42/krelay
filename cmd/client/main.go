package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/transport/spdy"

	"github.com/knight42/krelay/pkg/constants"
	"github.com/knight42/krelay/pkg/ports"
	"github.com/knight42/krelay/pkg/remoteaddr"
	slogutil "github.com/knight42/krelay/pkg/slog"
	"github.com/knight42/krelay/pkg/xnet"
)

type Options struct {
	getter genericclioptions.RESTClientGetter

	// serverImage is the image to use for the krelay-server.
	serverImage string
	// serverNamespace is the namespace in which krelay-server is located.
	serverNamespace string
	// address is the address to listen on.
	address string
	// targetsFile is the file containing the list of targets.
	targetsFile string

	verbosity int
}

// setKubernetesDefaults sets default values on the provided client config for accessing the Kubernetes API.
func setKubernetesDefaults(config *rest.Config) {
	// GroupVersion is required when initializing a RESTClient
	config.GroupVersion = &schema.GroupVersion{Group: "", Version: "v1"}

	if config.APIPath == "" {
		config.APIPath = "/api"
	}
	// NegotiatedSerializer is required when initializing a RESTClient
	if config.NegotiatedSerializer == nil {
		// This codec factory ensures the resources are not converted. Therefore, resources
		// will not be round-tripped through internal versions. Defaulting does not happen
		// on the client.
		config.NegotiatedSerializer = scheme.Codecs.WithoutConversion()
	}
}

func (o *Options) Run(ctx context.Context, args []string) error {
	ns, _, err := o.getter.ToRawKubeConfigLoader().Namespace()
	if err != nil {
		return fmt.Errorf("get namespace: %w", err)
	}

	var targets []target
	if len(o.targetsFile) > 0 {
		var fin *os.File
		if o.targetsFile == "-" {
			fin = os.Stdin
		} else {
			fin, err = os.Open(o.targetsFile)
			if err != nil {
				return err
			}
			defer fin.Close()
		}
		targets, err = parseTargetsFile(fin, ns)
		if err != nil {
			return err
		}
	} else {
		if len(args) < 2 {
			return errors.New("TYPE/NAME and list of ports are required for port-forward")
		}
		err := validateFields(args)
		if err != nil {
			return err
		}
		targets = []target{
			{
				resource:  args[0],
				ports:     args[1:],
				namespace: ns,
			},
		}
	}

	restCfg, err := o.getter.ToRESTConfig()
	if err != nil {
		return err
	}
	setKubernetesDefaults(restCfg)

	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return err
	}

	var portForwarders []*portForwarder

	for _, targetSpec := range targets {
		var addrGetter remoteaddr.Getter
		parser := ports.NewParser(targetSpec.ports)
		resParts := strings.Split(targetSpec.resource, "/")
		switch resParts[0] {
		case "ip":
			remoteAddr, err := xnet.AddrFromIP(resParts[1])
			if err != nil {
				return err
			}
			addrGetter = remoteaddr.NewStaticAddr(remoteAddr)

		case "host":
			addrGetter = remoteaddr.NewStaticAddr(xnet.AddrFromHost(resParts[1]))

		default:
			obj, err := resource.NewBuilder(o.getter).
				WithScheme(scheme.Scheme, scheme.Scheme.PrioritizedVersionsAllGroups()...).
				NamespaceParam(targetSpec.namespace).DefaultNamespace().
				ResourceNames("pods", targetSpec.resource).
				Do().Object()
			if err != nil {
				return err
			}

			addrGetter, err = addrGetterForObject(obj, cs, targetSpec.namespace)
			if err != nil {
				return err
			}
			parser = parser.WithObject(obj)
		}

		forwardPorts, err := parser.Parse()
		if err != nil {
			return err
		}
		for _, pp := range forwardPorts {
			portForwarders = append(portForwarders, newPortForwarder(addrGetter, pp))
		}
	}

	slog.Info("Creating krelay-server", slog.String("namespace", o.serverNamespace))
	svrPodName, err := createServerPod(ctx, cs, o.serverImage, o.serverNamespace)
	if err != nil {
		return fmt.Errorf("create krelay-server pod: %w", err)
	}
	defer removeServerPod(cs, o.serverNamespace, svrPodName, time.Minute)

	err = ensureServerPodIsRunning(ctx, cs, o.serverNamespace, svrPodName)
	if err != nil {
		return fmt.Errorf("ensure krelay-server is running: %w", err)
	}
	slog.Info("krelay-server is running", slog.String("pod", svrPodName), slog.String("namespace", o.serverNamespace))

	transport, upgrader, err := spdy.RoundTripperFor(restCfg)
	if err != nil {
		return err
	}

	restClient, err := rest.RESTClientFor(restCfg)
	if err != nil {
		return err
	}

	req := restClient.Post().
		Resource("pods").
		Namespace(o.serverNamespace).Name(svrPodName).
		SubResource("portforward")
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, req.URL())
	streamConn, _, err := dialer.Dial(constants.PortForwardProtocolV1Name)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer streamConn.Close()

	succeeded := false
	for _, pf := range portForwarders {
		err := pf.listen(o.address)
		if err != nil {
			slog.Error("Fail to listen on port", slog.Any("port", pf.ports.LocalPort), slog.Any("error", err))
		} else {
			succeeded = true
		}
		go pf.run(streamConn)
	}

	if !succeeded {
		return fmt.Errorf("unable to listen on any of the requested ports")
	}

	select {
	case <-streamConn.CloseChan():
		slog.Info("Lost connection to krelay-server pod")
	case <-ctx.Done():
	}

	return nil
}

func main() {
	cf := genericclioptions.NewConfigFlags(true)
	o := Options{
		getter: cf,
	}
	printVersion := false

	c := cobra.Command{
		Use:     fmt.Sprintf(`%s TYPE/NAME [options] [LOCAL_PORT:]REMOTE_PORT[@PROTOCOL] [...[LOCAL_PORT_N:]REMOTE_PORT_N[@PROTOCOL_N]]`, getProgramName()),
		Example: example(),
		Long: `This command is similar to "kubectl port-forward", but it also supports UDP and could forward data to a
service, ip and hostname rather than only pods.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if printVersion {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(struct {
					Version   string
					BuildDate string
					Commit    string
				}{
					Version:   version,
					BuildDate: date,
					Commit:    commit,
				})
			}

			slog.SetLogLoggerLevel(slogutil.MapVerbosityToLogLevel(o.verbosity))
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
			defer cancel()
			return o.Run(ctx, args)
		},
		SilenceUsage: true,
	}
	flags := c.Flags()
	flags.SortFlags = false
	flags.StringVar(cf.KubeConfig, "kubeconfig", *cf.KubeConfig, "Path to the kubeconfig file to use for CLI requests.")
	flags.StringVarP(cf.Namespace, "namespace", "n", *cf.Namespace, "If present, the namespace scope for this CLI request")
	flags.StringVar(cf.Context, "context", *cf.Context, "The name of the kubeconfig context to use")
	flags.StringVar(cf.ClusterName, "cluster", *cf.ClusterName, "The name of the kubeconfig cluster to use")

	flags.BoolVarP(&printVersion, "version", "V", false, "Print version info and exit.")
	flags.StringVar(&o.address, "address", "127.0.0.1", "Address to listen on. Only accepts IP addresses as a value.")
	flags.StringVarP(&o.targetsFile, "file", "f", "", "Forward to the targets specified in the given file, with one target per line.")
	flags.StringVar(&o.serverImage, "server.image", "ghcr.io/knight42/krelay-server:v0.0.4", "The krelay-server image to use.")
	flags.StringVar(&o.serverNamespace, "server.namespace", metav1.NamespaceDefault, "The namespace in which krelay-server is located.")
	flags.IntVarP(&o.verbosity, "v", "v", 3, "Number for the log level verbosity. The bigger the more verbose.")
	_ = c.Execute()
}
