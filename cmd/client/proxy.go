package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"time"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/knight42/krelay/pkg/constants"
	slogutil "github.com/knight42/krelay/pkg/slog"
	"github.com/knight42/krelay/pkg/xnet"
)

// socks5Handshake was excerpted from https://github.com/shadowsocks/go-shadowsocks2/blob/e1fe9ea737409e4d71efaa65e3caefa42a8fc188/socks/socks.go
func socks5Handshake(clientConn net.Conn) (ap xnet.AddrPort, err error) {
	// maxAddrLen is the maximum size of SOCKS address in bytes.
	const maxAddrLen = 256
	br := bytesReader{
		// Read RFC 1928 for request and reply structure and sizes.
		buf: make([]byte, maxAddrLen),
		r:   clientConn,
	}

	var data []byte
	// read VER, NMETHODS, METHODS
	data, err = br.ReadBytes(2)
	if err != nil {
		return
	}

	nmethods := data[1]
	_, err = br.ReadBytes(int(nmethods))
	if err != nil {
		return
	}

	// write VER METHOD
	// VERSION 5, METHOD 0 (no authentication)
	_, err = clientConn.Write([]byte{5, 0})
	if err != nil {
		return
	}

	// read VER CMD RSV ATYP DST.ADDR DST.PORT
	data, err = br.ReadBytes(3)
	if err != nil {
		return
	}
	cmd := data[1]

	data, err = br.ReadBytes(1) // read 1st byte for address type
	if err != nil {
		return
	}
	adrType := data[0]

	var addr xnet.Addr
	switch adrType {
	case 1: // IPv4
		data, err = br.ReadBytes(net.IPv4len)
		if err != nil {
			return
		}
		addr = xnet.AddrFromBytes(xnet.AddrTypeIP, copyBuffer(data))

	case 4: // IPv6
		data, err = br.ReadBytes(net.IPv6len)
		if err != nil {
			return
		}
		addr = xnet.AddrFromBytes(xnet.AddrTypeIP, copyBuffer(data))

	case 3: // Domain name
		data, err = br.ReadString() // read domain name
		if err != nil {
			return
		}
		addr = xnet.AddrFromBytes(xnet.AddrTypeHost, copyBuffer(data))

	default:
		// X'08' Address type not supported
		_, _ = clientConn.Write([]byte{5, 8, 0, 1, 0, 0, 0, 0, 0, 0})
		return ap, fmt.Errorf("unsupported address type: %d", adrType)
	}

	port, err := br.ReadUint16() // read DST.PORT
	if err != nil {
		return
	}

	switch cmd {
	case 1: // CONNECT
		_, err = clientConn.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
		if err != nil {
			return
		}

	default:
		// X'07' Command not supported
		_, _ = clientConn.Write([]byte{5, 7, 0, 1, 0, 0, 0, 0, 0, 0})
		return ap, fmt.Errorf("unsupported command: %d", cmd)
	}

	return xnet.AddrPortFrom(addr, port), nil
}

func handleSOCKS5Conn(clientConn net.Conn, serverConn httpstream.Connection) {
	ap, err := socks5Handshake(clientConn)
	if err != nil {
		slog.Error("Fail to handle SOCKS5 handshake", slogutil.Error(err))
		return
	}

	go handleTCPConn(clientConn, serverConn, ap)
}

func runSOCKS5Server(l net.Listener, streamConn httpstream.Connection) {
	slog.Info("SOCKS5 server is running", slog.String("address", l.Addr().String()))
	for {
		select {
		case <-streamConn.CloseChan():
			return
		default:
		}

		c, err := l.Accept()
		if err != nil {
			slog.Error("Fail to accept tcp connection", slogutil.Error(err))
			return
		}
		go handleSOCKS5Conn(c, streamConn)
	}
}

type proxyOptions struct {
	getter genericclioptions.RESTClientGetter

	// serverImage is the image to use for the krelay-server.
	serverImage string

	// patch is the literal MergePatch
	patch string
	// patchFile is the file containing the MergePatch
	patchFile string

	listenAddr string
}

func (o *proxyOptions) Run(ctx context.Context, _ []string) error {
	l, err := net.Listen("tcp", o.listenAddr)
	if err != nil {
		return err
	}
	defer l.Close()

	restCfg, err := o.getter.ToRESTConfig()
	if err != nil {
		return err
	}
	setKubernetesDefaults(restCfg)

	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return err
	}

	pb := newServerPodBuilder(o.serverImage).WithPatchBytes(o.patch).WithPatchFile(o.patchFile)
	svrPod, err := pb.Build()
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

	go runSOCKS5Server(l, streamConn)

	select {
	case <-streamConn.CloseChan():
		slog.Info("Lost connection to krelay-server pod")
	case <-ctx.Done():
	}

	return nil
}

func newProxyCommand() *cobra.Command {
	cf := genericclioptions.NewConfigFlags(true)
	o := proxyOptions{
		getter: cf,
	}
	cmd := &cobra.Command{
		Use:   "proxy",
		Short: "Run a SOCKS5 proxy server",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer cancel()
			return o.Run(ctx, args)
		},
	}
	flags := cmd.Flags()
	flags.SortFlags = false
	flags.StringVar(cf.KubeConfig, "kubeconfig", *cf.KubeConfig, "Path to the kubeconfig file to use for CLI requests.")
	flags.StringVarP(cf.Namespace, "namespace", "n", *cf.Namespace, "If present, the namespace scope for this CLI request")
	flags.StringVar(cf.Context, "context", *cf.Context, "The name of the kubeconfig context to use")
	flags.StringVar(cf.ClusterName, "cluster", *cf.ClusterName, "The name of the kubeconfig cluster to use")

	flags.StringVarP(&o.patch, "patch", "p", "", "The merge patch to be applied to the krelay-server pod.")
	flags.StringVar(&o.patchFile, "patch-file", "", "A file containing a merge patch to be applied to the krelay-server pod.")
	flags.StringVar(&o.serverImage, "server.image", "ghcr.io/knight42/krelay-server:v0.0.4", "The krelay-server image to use.")

	flags.StringVarP(&o.listenAddr, "listen", "l", "127.0.0.1:1080", "SOCKS5 proxy listen address")
	return cmd
}
