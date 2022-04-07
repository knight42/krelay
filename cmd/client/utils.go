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
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	"github.com/knight42/krelay/pkg/constants"
	"github.com/knight42/krelay/pkg/xnet"
)

func toPtr[T any](v T) *T {
	return &v
}

func makeLabels(ephemeral bool) map[string]string {
	kind := "ephemeral"
	if !ephemeral {
		kind = "persistent"
	}
	return map[string]string{
		"app.kubernetes.io/name": constants.ServerName,
		"kind":                   kind,
	}
}

func makeDeployment(svrImg string) *appsv1.Deployment {
	labelMap := makeLabels(false)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      constants.ServerName,
			Namespace: metav1.NamespaceDefault,
			Labels:    labelMap,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: toPtr[int32](3),
			Selector: &metav1.LabelSelector{
				MatchLabels: labelMap,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labelMap,
				},
				Spec: corev1.PodSpec{
					Affinity: &corev1.Affinity{
						PodAntiAffinity: &corev1.PodAntiAffinity{
							PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{
								{
									Weight: 100,
									PodAffinityTerm: corev1.PodAffinityTerm{
										LabelSelector: &metav1.LabelSelector{
											MatchLabels: labelMap,
										},
										TopologyKey: corev1.LabelHostname,
									},
								},
							},
						},
					},
					Containers: []corev1.Container{
						{
							Name:            constants.ServerName,
							Image:           svrImg,
							Args:            []string{"-v=4"},
							ImagePullPolicy: corev1.PullAlways,
						},
					},
				},
			},
		},
	}
}

func ensureServerDeployment(ctx context.Context, cs kubernetes.Interface, svrImg string) (string, error) {
	_, err := cs.AppsV1().Deployments(metav1.NamespaceDefault).Get(ctx, constants.ServerName, metav1.GetOptions{})
	if err != nil {
		if k8serr.IsNotFound(err) {
			klog.InfoS("Creating deployment krelay-server in default namespace")
			_, err = cs.AppsV1().Deployments(metav1.NamespaceDefault).Create(ctx, makeDeployment(svrImg), metav1.CreateOptions{})
			if err != nil && !k8serr.IsConflict(err) {
				return "", fmt.Errorf("create krelay-server: %w", err)
			}
		} else {
			return "", fmt.Errorf("get krelay-server: %w", err)
		}
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, time.Minute*5)
	defer cancel()

	w, err := cs.AppsV1().Deployments(metav1.NamespaceDefault).Watch(timeoutCtx, metav1.ListOptions{
		FieldSelector: fmt.Sprintf("metadata.name=%s", constants.ServerName),
	})
	if err != nil {
		return "", fmt.Errorf("watch krelay-server: %w", err)
	}

	var svrDeploy *appsv1.Deployment
	steady := false
loop:
	for ev := range w.ResultChan() {
		switch ev.Type {
		case watch.Deleted, watch.Error:
			break loop
		case watch.Modified, watch.Added:
		default:
			continue
		}
		svrDeploy = ev.Object.(*appsv1.Deployment)
		replicas := *svrDeploy.Spec.Replicas
		if replicas != svrDeploy.Status.UpdatedReplicas || replicas != svrDeploy.Status.ReadyReplicas {
			klog.V(3).InfoS("Server is not ready",
				"expectedReplicas", replicas,
				"updatedReplicas", svrDeploy.Status.UpdatedReplicas,
				"readyReplicas", svrDeploy.Status.ReadyReplicas,
			)
			continue
		}
		steady = true
		break
	}

	w.Stop()
	if !steady {
		return "", errors.New("krelay-server is not ready")
	}
	return ensureRunningPods(ctx, cs, svrDeploy.Spec.Selector.MatchLabels)
}

func ensureServerPod(ctx context.Context, cs kubernetes.Interface, svrImg string) (string, error) {
	klog.InfoS("Creating pod krelay-server in default namespace")
	name := fmt.Sprintf("%s-ephemeral", constants.ServerName)
	pod, err := cs.CoreV1().Pods(metav1.NamespaceDefault).Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:    metav1.NamespaceDefault,
			GenerateName: name + "-",
			Labels:       makeLabels(true),
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

	w, err := cs.CoreV1().Pods(metav1.NamespaceDefault).Watch(timeoutCtx, metav1.ListOptions{
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

func removeServerPod(cs kubernetes.Interface, podName string, timeout time.Duration) {
	klog.InfoS("Removing krelay-server pod", "pod", podName)

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	err := cs.CoreV1().Pods(metav1.NamespaceDefault).Delete(ctx, podName, metav1.DeleteOptions{
		GracePeriodSeconds: toPtr[int64](0),
	})
	if err != nil && !k8serr.IsNotFound(err) {
		klog.ErrorS(err, "Fail to remove krelay-server pod", "pod", podName)
	}
}

// ensureRunningPods makes sure the pods match the given labels are all running and returns one of them.
func ensureRunningPods(ctx context.Context, cs kubernetes.Interface, labelMap map[string]string) (string, error) {
	labelStr := labels.FormatLabels(labelMap)
	var podList *corev1.PodList
	err := wait.PollImmediateUntil(time.Second*10, func() (done bool, err error) {
		podList, err = cs.CoreV1().Pods(metav1.NamespaceDefault).List(ctx, metav1.ListOptions{
			LabelSelector: labelStr,
		})
		if err != nil {
			return false, fmt.Errorf("list pods: %w", err)
		}

		if len(podList.Items) == 0 {
			klog.V(4).InfoS("There are no pods. Will retry.")
			return false, nil
		}

		for _, pod := range podList.Items {
			if pod.Status.Phase != corev1.PodRunning {
				klog.V(4).InfoS("Pod is not running. Will retry.", "pod", pod.Name)
				return false, nil
			}
		}

		return true, nil
	}, ctx.Done())
	if err != nil {
		return "", err
	}
	return podList.Items[0].Name, nil
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
