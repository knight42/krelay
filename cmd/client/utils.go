package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	jsonpatch "github.com/evanphx/json-patch/v5"
	"github.com/spf13/pflag"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	k8serr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/apimachinery/pkg/util/json"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"

	"github.com/knight42/krelay/pkg/constants"
	"github.com/knight42/krelay/pkg/remoteaddr"
	slogutil "github.com/knight42/krelay/pkg/slog"
	"github.com/knight42/krelay/pkg/xnet"
)

func toPtr[T any](v T) *T {
	return &v
}

func copyBuffer(b []byte) []byte {
	c := make([]byte, len(b))
	copy(c, b)
	return c
}

func patchPod(patchBytes []byte, origPod corev1.Pod) (*corev1.Pod, error) {
	patchJSONBytes, err := yaml.ToJSON(patchBytes)
	if err != nil {
		return nil, fmt.Errorf("convert patch to json: %w", err)
	}

	origBytes, err := json.Marshal(origPod)
	if err != nil {
		return nil, fmt.Errorf("marshal pod: %w", err)
	}

	after, err := jsonpatch.MergePatch(origBytes, patchJSONBytes)
	if err != nil {
		return nil, fmt.Errorf("apply merge patch: %w", err)
	}

	var patchedPod corev1.Pod
	err = json.Unmarshal(after, &patchedPod)
	if err != nil {
		return nil, fmt.Errorf("unmarshal pod: %w", err)
	}

	return &patchedPod, nil
}

func ensureServerPodIsRunning(ctx context.Context, cs kubernetes.Interface, namespace, podName string) error {
	timeoutCtx, cancel := context.WithTimeout(ctx, time.Minute*5)
	defer cancel()

	w, err := cs.CoreV1().Pods(namespace).Watch(timeoutCtx, metav1.ListOptions{
		FieldSelector: fmt.Sprintf("metadata.name=%s", podName),
	})
	if err != nil {
		return fmt.Errorf("watch krelay-server pod: %w", err)
	}
	defer w.Stop()

	running := false
loop:
	for ev := range w.ResultChan() {
		switch ev.Type {
		case watch.Deleted, watch.Error:
			break loop
		case watch.Modified, watch.Added:
		default:
			continue
		}

		podObj := ev.Object.(*corev1.Pod)
		for _, status := range podObj.Status.ContainerStatuses {
			// there is only one container in the pod
			if status.State.Running != nil {
				running = true
				break loop
			}
			slog.Debug("Pod is not running. Will retry.", slog.String("pod", podObj.Name))
		}
	}
	if !running {
		return fmt.Errorf("krelay-server pod is not running")
	}

	return nil
}

func removeServerPod(cs kubernetes.Interface, namespace, podName string, timeout time.Duration) {
	l := slog.With(slog.String("pod", podName))
	l.Info("Removing krelay-server pod")

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	err := cs.CoreV1().Pods(namespace).Delete(ctx, podName, metav1.DeleteOptions{
		GracePeriodSeconds: toPtr[int64](0),
	})
	if err != nil && !k8serr.IsNotFound(err) {
		l.Error("Fail to remove krelay-server pod", slogutil.Error(err))
	}
}

func addrGetterForObject(obj runtime.Object, cs kubernetes.Interface, ns string) (remoteaddr.Getter, error) {
	switch actual := obj.(type) {
	case *corev1.Pod:
		addr, err := xnet.AddrFromIP(actual.Status.PodIP)
		if err != nil {
			return nil, err
		}
		return remoteaddr.NewStaticAddr(addr), nil

	case *corev1.Service:
		if actual.Spec.Type == corev1.ServiceTypeExternalName {
			addr := xnet.AddrFromHost(actual.Spec.ExternalName)
			return remoteaddr.NewStaticAddr(addr), nil
		}
		if actual.Spec.ClusterIP != corev1.ClusterIPNone {
			addr, err := xnet.AddrFromIP(actual.Spec.ClusterIP)
			if err != nil {
				return nil, err
			}
			return remoteaddr.NewStaticAddr(addr), nil
		}

		if len(actual.Spec.Selector) == 0 {
			return nil, fmt.Errorf("service selector is empty")
		}

		selector := labels.SelectorFromSet(actual.Spec.Selector)
		return remoteaddr.NewDynamicAddr(cs, ns, selector.String())

	case *appsv1.ReplicaSet:
		selector, err := metav1.LabelSelectorAsSelector(actual.Spec.Selector)
		if err != nil {
			return nil, err
		}
		return remoteaddr.NewDynamicAddr(cs, ns, selector.String())

	case *appsv1.Deployment:
		selector, err := metav1.LabelSelectorAsSelector(actual.Spec.Selector)
		if err != nil {
			return nil, err
		}
		return remoteaddr.NewDynamicAddr(cs, ns, selector.String())

	case *appsv1.StatefulSet:
		selector, err := metav1.LabelSelectorAsSelector(actual.Spec.Selector)
		if err != nil {
			return nil, err
		}
		return remoteaddr.NewDynamicAddr(cs, ns, selector.String())

	case *appsv1.DaemonSet:
		selector, err := metav1.LabelSelectorAsSelector(actual.Spec.Selector)
		if err != nil {
			return nil, err
		}
		return remoteaddr.NewDynamicAddr(cs, ns, selector.String())
	}

	return nil, fmt.Errorf("unknown object: %T", obj)
}

func createStream(c httpstream.Connection, reqID string) (dataStream httpstream.Stream, errCh chan error, err error) {
	// create error stream
	headers := http.Header{}
	headers.Set(corev1.StreamType, corev1.StreamTypeError)
	headers.Set(corev1.PortHeader, strconv.Itoa(constants.ServerPort))
	headers.Set(corev1.PortForwardRequestIDHeader, reqID)
	errStream, err := c.CreateStream(headers)
	if err != nil {
		return nil, nil, errors.New("create error stream")
	}
	// we're not writing to this stream
	_ = errStream.Close()

	// create data stream
	headers.Set(corev1.StreamType, corev1.StreamTypeData)
	dataStream, err = c.CreateStream(headers)
	if err != nil {
		return nil, nil, fmt.Errorf("create data stream: %w", err)
	}

	errCh = make(chan error)
	go func() {
		message, err := io.ReadAll(errStream)
		errMsg := string(message)
		switch {
		case err != nil:
			errMsg = err.Error()
			errCh <- fmt.Errorf("error reading from error stream: %w", err)
		case len(message) > 0:
			errCh <- fmt.Errorf("an error occurred forwarding: %v", errMsg)
		}
		close(errCh)

		// check if the spdy connection is corrupted
		if ok, _ := filepath.Match("*network namespace for sandbox * is closed", errMsg); ok {
			_ = c.Close()
		}
	}()

	return dataStream, errCh, nil
}

func validateFields(fields []string) error {
	if len(fields) < 2 {
		return fmt.Errorf("invalid syntax")
	}

	resourceParts := strings.Split(fields[0], "/")
	if len(resourceParts) > 2 {
		return fmt.Errorf("unknown resource: %q", fields[0])
	}

	if resourceParts[0] == "ip" {
		isInvalid := net.ParseIP(resourceParts[1]) == nil
		if isInvalid {
			return fmt.Errorf("invalid IP address: %q", resourceParts[1])
		}
	}
	return nil
}

type target struct {
	resource  string
	ports     []string
	namespace string
	lisAddr   string
}

func parseTargetsFile(r io.Reader, defaultNamespace string) ([]target, error) {
	fs := pflag.NewFlagSet("targets", pflag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var (
		ns         string
		listenAddr string
	)
	const defaultListenAddr = "127.0.0.1"

	fs.StringVarP(&ns, "namespace", "n", defaultNamespace, "namespace")
	fs.StringVarP(&listenAddr, "address", "l", defaultListenAddr, "listen address")

	s := bufio.NewScanner(r)
	var ret []target
	lineNo := 0

	for s.Scan() {
		lineNo++
		line := strings.TrimSpace(s.Text())
		if len(line) == 0 || strings.HasPrefix(line, "//") || strings.HasPrefix(line, "#") {
			continue
		}

		fields := strings.Fields(line)
		// We need to reset the state before parsing the next line
		ns = defaultNamespace
		listenAddr = defaultListenAddr
		err := fs.Parse(fields)
		if err != nil {
			return nil, fmt.Errorf("line: %d: %w", lineNo, err)
		}
		remain := fs.Args()
		err = validateFields(remain)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNo, err)
		}
		ret = append(ret, target{
			resource:  remain[0],
			ports:     remain[1:],
			namespace: ns,
			lisAddr:   listenAddr,
		})
	}
	return ret, nil
}

func createDialer(restCfg *rest.Config, dstURL *url.URL) (httpstream.Dialer, error) {
	// Excerpt from https://github.com/kubernetes/kubernetes/blob/f5c538418189e119a8dbb60e2a2b22394548e326/staging/src/k8s.io/kubectl/pkg/cmd/portforward/portforward.go#L139
	transport, upgrader, err := spdy.RoundTripperFor(restCfg)
	if err != nil {
		return nil, err
	}
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, dstURL)

	if strings.ToLower(os.Getenv("KUBECTL_PORT_FORWARD_WEBSOCKETS")) != "false" {
		slog.Debug("Trying to forward ports using websocket")

		tunnelDialer, err := portforward.NewSPDYOverWebsocketDialer(dstURL, restCfg)
		if err != nil {
			return nil, fmt.Errorf("create tunneling dialer: %w", err)
		}
		dialer = portforward.NewFallbackDialer(tunnelDialer, dialer, func(err error) bool {
			shouldFallback := httpstream.IsUpgradeFailure(err) || httpstream.IsHTTPSProxyError(err)
			if shouldFallback {
				slog.Debug("Websocket upgrade failed, falling back to SPDY")
			}
			return shouldFallback
		})
	}

	return dialer, nil
}

type serverPodBuilder struct {
	image string

	patchFile  string
	patchBytes []byte
}

func newServerPodBuilder(image string) *serverPodBuilder {
	return &serverPodBuilder{
		image: image,
	}
}

func (b *serverPodBuilder) WithPatchBytes(p string) *serverPodBuilder {
	b.patchBytes = []byte(p)
	return b
}

func (b *serverPodBuilder) WithPatchFile(fp string) *serverPodBuilder {
	b.patchFile = fp
	return b
}

func (b *serverPodBuilder) Build() (*corev1.Pod, error) {
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
					Image:           b.image,
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
	if len(b.patchBytes) == 0 && len(b.patchFile) == 0 {
		return &origPod, nil
	}

	patchBytes := b.patchBytes
	if len(b.patchFile) > 0 {
		var err error
		patchBytes, err = os.ReadFile(b.patchFile)
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

type bytesReader struct {
	r   io.Reader
	buf []byte
}

func (b *bytesReader) ReadBytes(n int) ([]byte, error) {
	_, err := io.ReadFull(b.r, b.buf[:n])
	if err != nil {
		return nil, err
	}
	return b.buf[:n], nil
}

func (b *bytesReader) ReadUint16() (uint16, error) {
	data, err := b.ReadBytes(2)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint16(data), nil
}

func (b *bytesReader) ReadString() ([]byte, error) {
	data, err := b.ReadBytes(1)
	if err != nil {
		return nil, err
	}
	length := int(data[0])
	return b.ReadBytes(length)
}
