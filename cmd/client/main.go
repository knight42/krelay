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

	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/klog/v2"

	"github.com/knight42/krelay/pkg/kube"
	"github.com/knight42/krelay/pkg/ports"
	"github.com/knight42/krelay/pkg/remoteaddr"
	slogutil "github.com/knight42/krelay/pkg/slog"
	"github.com/knight42/krelay/pkg/xnet"
)

type Options struct {
	kf *kube.Flags

	// address is the address to listen on.
	address string
	// targetsFile is the file containing the list of targets.
	targetsFile string

	verbosity int
}

func (o *Options) Run(ctx context.Context, args []string) error {
	ns, _, err := o.kf.GetNamespace()
	if err != nil {
		return fmt.Errorf("get namespace: %w", err)
	}

	var targets []target
	if len(o.targetsFile) > 0 {
		if len(args) != 0 {
			return errors.New("target file and TYPE/NAME with ports cannot be specified at the same time")
		}

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
				lisAddr:   o.address,
			},
		}
	}

	cs, err := o.kf.ToClientSet()
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
			obj, err := o.kf.ToResourceBuilder().
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
			portForwarders = append(portForwarders, &portForwarder{
				addrGetter: addrGetter,
				ports:      pp,
				listenAddr: targetSpec.lisAddr,
			})
		}
	}

	succeeded := false
	for _, pf := range portForwarders {
		err := pf.listen()
		if err != nil {
			slog.Error("Fail to bind address", slogutil.Error(err))
		} else {
			succeeded = true
		}
	}
	if !succeeded {
		return fmt.Errorf("unable to listen on any of the requested ports")
	}

	createdPod, err := o.kf.RunServerPod(ctx)
	if err != nil {
		return err
	}
	defer createdPod.Close()

	streamConn := createdPod.StreamConn()
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
	kf := kube.NewFlags()
	o := Options{
		kf: kf,
	}
	printVersion := false

	fs := flag.NewFlagSet("klog", flag.ExitOnError)
	klog.InitFlags(fs)

	c := cobra.Command{
		Use:     fmt.Sprintf(`%s TYPE/NAME [options] [LOCAL_PORT:]REMOTE_PORT[@PROTOCOL] [...[LOCAL_PORT_N:]REMOTE_PORT_N[@PROTOCOL_N]]`, getProgramName()),
		Example: example(),
		Args:    cobra.ArbitraryArgs,
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
	kf.AddFlags(c.PersistentFlags())

	c.Flags().SortFlags = false
	flags := c.LocalFlags()
	flags.BoolVarP(&printVersion, "version", "V", false, "Print version info and exit.")
	flags.StringVarP(&o.address, "address", "l", "127.0.0.1", "Address to listen on. Only accepts IP addresses as a value.")
	flags.StringVarP(&o.targetsFile, "file", "f", "", "Forward to the targets specified in the given file, with one target per line.")
	flags.IntVarP(&o.verbosity, "v", "v", 3, "Number for the log level verbosity. The bigger the more verbose.")

	c.AddCommand(
		newProxyCommand(kf),
	)
	_ = c.Execute()
}
