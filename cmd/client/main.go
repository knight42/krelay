package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"strings"

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
	"github.com/knight42/krelay/pkg/xnet"
)

type Options struct {
	serverImage string
	getter      genericclioptions.RESTClientGetter
	address     string
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

	var remoteAddr xnet.Addr
	parser := ports.NewParser(args[1:])
	switch parts[0] {
	case "ip":
		remoteAddr, err = xnet.AddrFromIP(parts[1])
		if err != nil {
			return err
		}

	case "host":
		remoteAddr = xnet.Addr{Type: xnet.AddrTypeHost, Host: parts[1]}

	default:
		obj, err := resource.NewBuilder(o.getter).
			WithScheme(scheme.Scheme, scheme.Scheme.PrioritizedVersionsAllGroups()...).
			NamespaceParam(ns).DefaultNamespace().
			ResourceNames("pods", args[0]).
			Do().Object()
		if err != nil {
			return err
		}

		remoteAddr, err = getAddrForObject(ctx, cs, obj)
		if err != nil {
			return err
		}

		parser = parser.WithObject(obj)
	}

	forwardPorts, err := parser.Parse()
	if err != nil {
		return err
	}

	klog.InfoS("Check if krelay-server exists")
	svrPodName, err := ensureServer(ctx, cs, o.serverImage)
	if err != nil {
		return fmt.Errorf("ensure krelay-server: %w", err)
	}
	klog.V(4).InfoS("Got server pod", "name", svrPodName)

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
		Namespace(metav1.NamespaceDefault).Name(svrPodName).
		SubResource("portforward")
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, req.URL())
	streamConn, _, err := dialer.Dial(constants.PortForwardProtocolV1Name)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer streamConn.Close()

	succeeded := false
	for _, pp := range forwardPorts {
		pf := newPortForwarder(o.address, remoteAddr, pp)
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
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			if printVersion {
				fmt.Printf("Client version: %s\n", constants.ClientVersion)
				return nil
			}
			return o.Run(context.Background(), args)
		},
		SilenceUsage: true,
	}
	flags := c.Flags()
	flags.AddGoFlagSet(flag.CommandLine)
	cf.AddFlags(flags)
	flags.BoolVarP(&printVersion, "version", "V", false, "Print version info and exit.")
	flags.StringVar(&o.address, "address", "127.0.0.1", "Address to listen on. Only accepts IP addresses as a value.")
	flags.StringVar(&o.serverImage, "server-image", constants.ServerImage, "The krelay-server image to use. If the krelay-server deployment does not exist, it will be automatically created using this image.")
	_ = c.Execute()
}
