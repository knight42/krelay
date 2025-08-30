package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"

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
	// address is the address to listen on.
	address string
	// targetsFile is the file containing the list of targets.
	targetsFile string

	// patch is the literal MergePatch
	patch string
	// patchFile is the file containing the MergePatch
	patchFile string

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

func (o *Options) newServerPod() (*corev1.Pod, error) {
	origPod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:    metav1.NamespaceDefault,
			GenerateName: constants.ServerName + "-",
			Labels: map[string]string{
				"app.kubernetes.io/name": constants.ServerName,
				"app":                    constants.ServerName,
			},
			Annotations: map[string]string{
				"cluster-autoscaler.kubernetes.io/safe-to-evict": "true",
			},
		},
		Spec: corev1.PodSpec{
			AutomountServiceAccountToken: toPtr(false),
			EnableServiceLinks:           toPtr(false),
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot: toPtr(true),
			},
			Containers: []corev1.Container{
				{
					Name:            constants.ServerName,
					Image:           o.serverImage,
					ImagePullPolicy: corev1.PullAlways,
					SecurityContext: &corev1.SecurityContext{
						ReadOnlyRootFilesystem:   toPtr(true),
						AllowPrivilegeEscalation: toPtr(false),
					},
				},
			},
			TopologySpreadConstraints: []corev1.TopologySpreadConstraint{
				{
					MaxSkew:           1,
					TopologyKey:       "kubernetes.io/hostname",
					WhenUnsatisfiable: corev1.ScheduleAnyway,
					LabelSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"app": constants.ServerName,
						},
					},
				},
			},
		},
	}
	if len(o.patch) == 0 && len(o.patchFile) == 0 {
		return &origPod, nil
	}

	patchBytes := []byte(o.patch)
	if len(o.patchFile) > 0 {
		var err error
		patchBytes, err = os.ReadFile(o.patchFile)
		if err != nil {
			return nil, fmt.Errorf("read file: %w", err)
		}
	}

	patched, err := patchPod(patchBytes, origPod)
	if err != nil {
		return nil, fmt.Errorf("patch server pod: %w", err)
	}

	return patched, nil
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

	succeeded := false
	for _, pf := range portForwarders {
		err := pf.listen(o.address)
		if err != nil {
			slog.Error("Fail to listen on port", slog.Any("port", pf.ports.LocalPort), slog.Any("error", err))
		} else {
			succeeded = true
		}
	}
	if !succeeded {
		return fmt.Errorf("unable to listen on any of the requested ports")
	}

	svrPod, err := o.newServerPod()
	if err != nil {
		return err
	}

	slog.Info("Creating krelay-server", slog.String("namespace", svrPod.Namespace))
	createdPod, err := cs.CoreV1().Pods(svrPod.Namespace).Create(ctx, svrPod, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("create krelay-server pod: %w", err)
	}
	svrPodName := createdPod.Name
	defer removeServerPod(cs, svrPod.Namespace, svrPodName, time.Minute)

	err = ensureServerPodIsRunning(ctx, cs, svrPod.Namespace, svrPodName)
	if err != nil {
		return fmt.Errorf("ensure krelay-server is running: %w", err)
	}
	slog.Info("krelay-server is running", slog.String("pod", svrPodName), slog.String("namespace", svrPod.Namespace))

	restClient, err := rest.RESTClientFor(restCfg)
	if err != nil {
		return err
	}

	req := restClient.Post().
		Resource("pods").
		Namespace(svrPod.Namespace).Name(svrPodName).
		SubResource("portforward")

	dialer, err := createDialer(restCfg, req.URL())
	if err != nil {
		return fmt.Errorf("create dialer: %w", err)
	}

	streamConn, _, err := dialer.Dial(constants.PortForwardProtocolV1Name)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer streamConn.Close()

	for _, pf := range portForwarders {
		go pf.run(streamConn)
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

	fs := flag.NewFlagSet("klog", flag.ExitOnError)
	klog.InitFlags(fs)

	c := cobra.Command{
		Use:     fmt.Sprintf(`%s TYPE/NAME [options] [LOCAL_PORT:]REMOTE_PORT[@PROTOCOL] [...[LOCAL_PORT_N:]REMOTE_PORT_N[@PROTOCOL_N]]`, getProgramName()),
		Example: example(),
		Long: `This command is similar to "kubectl port-forward", but it also supports UDP and could forward data to a
service, ip and hostname rather than only pods.

Starting from version v0.1.2, it attempts to tunnel SPDY through websocket, in line with how "kubectl port-forward" works.
This behavior can be disabled by setting the environment variable "KUBECTL_PORT_FORWARD_WEBSOCKETS" to "false".`,
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

			_ = fs.Set("v", strconv.Itoa(o.verbosity))
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
	flags.IntVarP(&o.verbosity, "v", "v", 3, "Number for the log level verbosity. The bigger the more verbose.")
	flags.StringVarP(&o.patch, "patch", "p", "", "The merge patch to be applied to the krelay-server pod.")
	flags.StringVar(&o.patchFile, "patch-file", "", "A file containing a merge patch to be applied to the krelay-server pod.")
	flags.StringVar(&o.serverImage, "server.image", "ghcr.io/knight42/krelay-server:v0.0.4", "The krelay-server image to use.")

	_ = c.Execute()
}
