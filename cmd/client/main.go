package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
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
	"k8s.io/klog/v2"

	"github.com/knight42/krelay/pkg/constants"
	"github.com/knight42/krelay/pkg/ports"
	"github.com/knight42/krelay/pkg/remoteaddr"
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
	if len(args) < 2 {
		return errors.New("TYPE/NAME and list of ports are required for port-forward")
	}

	ns, _, err := o.getter.ToRawKubeConfigLoader().Namespace()
	if err != nil {
		return fmt.Errorf("get namespace: %w", err)
	}

	restCfg, err := o.getter.ToRESTConfig()
	if err != nil {
		return err
	}
	setKubernetesDefaults(restCfg)

	parts := strings.Split(args[0], "/")
	if len(parts) > 2 {
		return fmt.Errorf("unknown resource: %s", args[0])
	}

	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return err
	}

	var addrGetter remoteaddr.Getter
	parser := ports.NewParser(args[1:])
	switch parts[0] {
	case "ip":
		remoteAddr, err := xnet.AddrFromIP(parts[1])
		if err != nil {
			return err
		}
		addrGetter = remoteaddr.NewStaticAddr(remoteAddr)

	case "host":
		addrGetter = remoteaddr.NewStaticAddr(xnet.AddrFromHost(parts[1]))

	default:
		obj, err := resource.NewBuilder(o.getter).
			WithScheme(scheme.Scheme, scheme.Scheme.PrioritizedVersionsAllGroups()...).
			NamespaceParam(ns).DefaultNamespace().
			ResourceNames("pods", args[0]).
			Do().Object()
		if err != nil {
			return err
		}

		remoteAddr, err := getAddrForObject(obj)
		if err != nil {
			return err
		}

		if remoteAddr.IsZero() {
			selector, err := selectorForObject(obj)
			if err != nil {
				return err
			}
			addrGetter, err = remoteaddr.NewDynamicAddr(cs, ns, selector.String())
			if err != nil {
				return err
			}
		} else {
			addrGetter = remoteaddr.NewStaticAddr(remoteAddr)
		}

		parser = parser.WithObject(obj)
	}

	forwardPorts, err := parser.Parse()
	if err != nil {
		return err
	}

	klog.InfoS("Creating krelay-server", "namespace", o.serverNamespace)
	svrPodName, err := ensureServerPod(ctx, cs, o.serverImage, o.serverNamespace)
	if err != nil {
		return fmt.Errorf("ensure krelay-server: %w", err)
	}
	defer removeServerPod(cs, svrPodName, o.serverNamespace, time.Minute)
	klog.InfoS("krelay-server is running", "pod", svrPodName, "namespace", o.serverNamespace)

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
	for _, pp := range forwardPorts {
		pf := newPortForwarder(o.address, addrGetter, pp)
		err := pf.listen()
		if err != nil {
			klog.ErrorS(err, "Fail to listen on port", "port", pp.LocalPort)
		} else {
			succeeded = true
		}
		go pf.run(streamConn)
	}

	if !succeeded {
		return fmt.Errorf("unable to listen on any of the requested ports: %v", forwardPorts)
	}

	select {
	case <-streamConn.CloseChan():
		klog.InfoS("Lost connection to krelay-server pod")
	case <-ctx.Done():
	}

	return nil
}

func main() {
	klog.InitFlags(nil)
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

			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
			defer cancel()
			return o.Run(ctx, args)
		},
		SilenceUsage: true,
	}
	flags := c.Flags()
	flags.AddGoFlagSet(flag.CommandLine)
	cf.AddFlags(flags)
	flags.BoolVarP(&printVersion, "version", "V", false, "Print version info and exit.")
	flags.StringVar(&o.address, "address", "127.0.0.1", "Address to listen on. Only accepts IP addresses as a value.")
	flags.StringVar(&o.serverImage, "server.image", constants.ServerImage, "The krelay-server image to use.")
	flags.StringVar(&o.serverNamespace, "server.namespace", metav1.NamespaceDefault, "The namespace in which krelay-server is located.")

	// I do not want these flags to show up in --help.
	hiddenFlags := []string{
		"add_dir_header",
		"log_flush_frequency",
		"alsologtostderr",
		"log_backtrace_at",
		"log_dir",
		"log_file",
		"log_file_max_size",
		"one_output",
		"logtostderr",
		"skip_headers",
		"skip_log_headers",
		"stderrthreshold",
		"vmodule",
	}
	for _, flagName := range hiddenFlags {
		_ = flags.MarkHidden(flagName)
	}

	_ = c.Execute()
}
