package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

func makeDeployment(svrImg string) *appsv1.Deployment {
	labelMap := map[string]string{"app": constants.ServerName}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      constants.ServerName,
			Namespace: metav1.NamespaceDefault,
		},
		Spec: appsv1.DeploymentSpec{
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

func watchServer(ctx context.Context, cs kubernetes.Interface) (watch.Interface, error) {
	return cs.AppsV1().Deployments(metav1.NamespaceDefault).Watch(ctx, metav1.ListOptions{
		FieldSelector: fmt.Sprintf("metadata.name=%s", constants.ServerName),
	})
}

func ensureServer(ctx context.Context, cs kubernetes.Interface, svrImg string) (string, error) {
	labelMap := map[string]string{"app": constants.ServerName}
	_, err := cs.AppsV1().Deployments(metav1.NamespaceDefault).Get(ctx, constants.ServerName, metav1.GetOptions{})
	if err != nil {
		if k8serr.IsNotFound(err) {
			klog.V(4).InfoS("Creating deployment krelay-server in default namespace")
			_, err = cs.AppsV1().Deployments(metav1.NamespaceDefault).Create(ctx, makeDeployment(svrImg), metav1.CreateOptions{})
			if err != nil && !k8serr.IsConflict(err) {
				return "", fmt.Errorf("create krelay-server: %w", err)
			}
		} else {
			return "", fmt.Errorf("get krelay-server: %w", err)
		}
	}
	w, err := watchServer(ctx, cs)
	if err != nil {
		return "", fmt.Errorf("watch krelay-server: %w", err)
	}

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
		d := ev.Object.(*appsv1.Deployment)
		replicas := *d.Spec.Replicas
		if replicas != d.Status.UpdatedReplicas || replicas != d.Status.ReadyReplicas {
			klog.V(3).InfoS("Server is not ready",
				"expectedReplicas", replicas,
				"updatedReplicas", d.Status.UpdatedReplicas,
				"readyReplicas", d.Status.ReadyReplicas,
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
	return ensureRunningPods(ctx, cs, labelMap)
}

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

func getAddrForObject(ctx context.Context, cs kubernetes.Interface, obj runtime.Object) (addr xnet.Addr, err error) {
	switch actual := obj.(type) {
	case *corev1.Pod:
		return xnet.AddrFromIP(actual.Status.PodIP)

	case *corev1.Service:
		if actual.Spec.Type == corev1.ServiceTypeExternalName {
			return xnet.Addr{Type: xnet.AddrTypeHost, Host: actual.Spec.ExternalName}, nil
		}
		if actual.Spec.ClusterIP != corev1.ClusterIPNone {
			return xnet.AddrFromIP(actual.Spec.ClusterIP)
		}

		if len(actual.Spec.Selector) == 0 {
			return addr, fmt.Errorf("service selector must not be empty")
		}
	}

	selector, err := selectorForObject(obj)
	if err != nil {
		return xnet.Addr{}, err
	}

	ns := obj.(metav1.Object).GetNamespace()
	podList, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
	if err != nil {
		return addr, fmt.Errorf("fail to list pods: %w", err)
	}

	for _, pod := range podList.Items {
		if pod.Status.Phase == corev1.PodRunning {
			return xnet.AddrFromIP(pod.Status.PodIP)
		}
	}

	return addr, fmt.Errorf("no healthy pods found")
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

func getProgramName() string {
	if strings.HasPrefix(filepath.Base(os.Args[0]), "kubectl-") {
		return "kubectl relay"
	}
	return "krelay"
}
