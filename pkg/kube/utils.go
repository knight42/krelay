package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	jsonpatch "github.com/evanphx/json-patch/v5"
	corev1 "k8s.io/api/core/v1"
	k8serr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"

	slogutil "github.com/knight42/krelay/pkg/slog"
)

func toPtr[T any](v T) *T {
	return &v
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
