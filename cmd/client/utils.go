package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	k8serr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	"github.com/knight42/krelay/pkg/constants"
	"github.com/knight42/krelay/pkg/xnet"
)

func toPtr[T any](v T) *T {
	return &v
}

func ensureServerPod(ctx context.Context, cs kubernetes.Interface, svrImg, namespace string) (string, error) {
	pod, err := cs.CoreV1().Pods(namespace).Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:    namespace,
			GenerateName: constants.ServerName + "-",
			Labels: map[string]string{
				"app.kubernetes.io/name": constants.ServerName,
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:            constants.ServerName,
					Image:           svrImg,
					Args:            []string{"-v=4"},
					ImagePullPolicy: corev1.PullAlways,
				},
			},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("create krelay-server pod: %w", err)
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, time.Minute*5)
	defer cancel()

	w, err := cs.CoreV1().Pods(namespace).Watch(timeoutCtx, metav1.ListOptions{
		FieldSelector: fmt.Sprintf("metadata.name=%s", pod.Name),
	})
	if err != nil {
		return "", fmt.Errorf("watch krelay-server pod: %w", err)
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
			klog.V(4).InfoS("Pod is not running. Will retry.", "pod", podObj.Name)
		}
	}
	if !running {
		return "", fmt.Errorf("krelay-server pod is not running")
	}

	return pod.Name, nil
}

func removeServerPod(cs kubernetes.Interface, podName, namespace string, timeout time.Duration) {
	klog.InfoS("Removing krelay-server pod", "pod", podName)

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	err := cs.CoreV1().Pods(namespace).Delete(ctx, podName, metav1.DeleteOptions{
		GracePeriodSeconds: toPtr[int64](0),
	})
	if err != nil && !k8serr.IsNotFound(err) {
		klog.ErrorS(err, "Fail to remove krelay-server pod", "pod", podName)
	}
}

func getAddrForObject(obj runtime.Object) (addr xnet.Addr, err error) {
	switch actual := obj.(type) {
	case *corev1.Pod:
		return xnet.AddrFromIP(actual.Status.PodIP)

	case *corev1.Service:
		if actual.Spec.Type == corev1.ServiceTypeExternalName {
			return xnet.AddrFromHost(actual.Spec.ExternalName), nil
		}
		if actual.Spec.ClusterIP != corev1.ClusterIPNone {
			return xnet.AddrFromIP(actual.Spec.ClusterIP)
		}

		if len(actual.Spec.Selector) == 0 {
			return addr, fmt.Errorf("service selector is empty")
		}
	}

	return xnet.Addr{}, nil
}

func selectorForObject(obj runtime.Object) (labels.Selector, error) {
	switch actual := obj.(type) {
	case *corev1.Service:
		return labels.SelectorFromSet(actual.Spec.Selector), nil

	case *appsv1.ReplicaSet:
		return metav1.LabelSelectorAsSelector(actual.Spec.Selector)

	case *appsv1.Deployment:
		return metav1.LabelSelectorAsSelector(actual.Spec.Selector)

	case *appsv1.StatefulSet:
		return metav1.LabelSelectorAsSelector(actual.Spec.Selector)

	case *appsv1.DaemonSet:
		return metav1.LabelSelectorAsSelector(actual.Spec.Selector)
	default:
		return nil, fmt.Errorf("selector for %T not implemented", obj)
	}
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
