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
	"github.com/knight42/krelay/pkg/xnet"
)

type Options struct {
	serverImage string
	getter      genericclioptions.RESTClientGetter
	address     string
}

// setKubernetesDefaults sets default values on the provided client config for accessing the
// Kubernetes API or returns an error if any of the defaults are impossible or invalid.
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
	}

	klog.V(4).InfoS("Ensure server")
	svrPodName, err := ensureServer(ctx, cs, o.serverImage)
	if err != nil {
		return fmt.Errorf("ensure krelay-server: %w", err)
	}
	klog.V(4).InfoS("Got server pod", "serverPod", svrPodName)

	transport, upgrader, err := spdy.RoundTripperFor(restCfg)
	if err != nil {
		return err
	}

	restClient, err := rest.RESTClientFor(restCfg)
	if err != nil {
		return err
	}

	forwardPorts, err := parsePorts(args[1:])
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
			klog.ErrorS(err, "Fail to bind address")
		} else {
			succeeded = true
		}
		go pf.run(streamConn)
	}

	if !succeeded {
		klog.Fatalf("Fail to listen on any of the requested ports: %v", forwardPorts)
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
	c := cobra.Command{
		Use: getProgramName(),
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			return o.Run(context.Background(), args)
		},
		SilenceUsage: true,
	}
	flags := c.Flags()
	flags.AddGoFlagSet(flag.CommandLine)
	cf.AddFlags(flags)
	flags.StringVar(&o.address, "address", "127.0.0.1", "Address to listen on. Only accepts IP addresses as a value.")
	flags.StringVar(&o.serverImage, "server-image", "knight42/krelay-server:latest", "krelay server image to use")
	_ = c.Execute()
}
