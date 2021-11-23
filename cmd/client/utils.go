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
	batchv1 "k8s.io/api/batch/v1"
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

	w, err := watchServer(ctx, cs)
	if err != nil {
		if k8serr.IsNotFound(err) {
			_, err = cs.AppsV1().Deployments(metav1.NamespaceDefault).Create(ctx, makeDeployment(svrImg), metav1.CreateOptions{})
			if err != nil && !k8serr.IsConflict(err) {
				return "", fmt.Errorf("create krelay-server: %w", err)
			}
		} else {
			return "", fmt.Errorf("get krelay-server: %w", err)
		}
		w, err = watchServer(ctx, cs)
		if err != nil {
			return "", fmt.Errorf("watch krelay-server: %w", err)
		}
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
			klog.InfoS("server is not ready",
				"expectedReplicas", replicas,
				"updatedReplicas", d.Status.UpdatedReplicas,
				"readyReplicas", d.Status.ReadyReplicas,
			)
			continue
		}
		steady = true
		break
	}

	if !steady {
		w.Stop()
		return "", errors.New("krelay-server is not ready")
	}
	w.Stop()
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
	var selector labels.Selector

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
		selector = labels.SelectorFromSet(actual.Spec.Selector)

	case *appsv1.ReplicaSet:
		selector, err = metav1.LabelSelectorAsSelector(actual.Spec.Selector)
		if err != nil {
			return addr, fmt.Errorf("invalid label selector: %w", err)
		}

	case *appsv1.Deployment:
		selector, err = metav1.LabelSelectorAsSelector(actual.Spec.Selector)
		if err != nil {
			return addr, fmt.Errorf("invalid label selector: %w", err)
		}

	case *appsv1.StatefulSet:
		selector, err = metav1.LabelSelectorAsSelector(actual.Spec.Selector)
		if err != nil {
			return addr, fmt.Errorf("invalid label selector: %w", err)
		}

	case *appsv1.DaemonSet:
		selector, err = metav1.LabelSelectorAsSelector(actual.Spec.Selector)
		if err != nil {
			return addr, fmt.Errorf("invalid label selector: %w", err)
		}

	case *batchv1.Job:
		selector, err = metav1.LabelSelectorAsSelector(actual.Spec.Selector)
		if err != nil {
			return addr, fmt.Errorf("invalid label selector: %w", err)
		}

	default:
		return addr, fmt.Errorf("selector for %T not implemented", obj)
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

type portPair struct {
	LocalPort  uint16
	RemotePort uint16
	Protocol   string
}

func parsePort(s string) (uint16, error) {
	port, err := strconv.ParseUint(s, 10, 16)
	if err != nil {
		return 0, fmt.Errorf("invalid port: %s", s)
	}
	return uint16(port), nil
}

func parsePorts(args []string) ([]portPair, error) {
	ret := make([]portPair, len(args))
	for i, arg := range args {
		proto := "tcp"
		protoIdx := strings.IndexRune(arg, '@')
		if protoIdx > 0 {
			if protoIdx < len(arg)-1 {
				proto = arg[protoIdx+1:]
				switch proto {
				case "tcp", "udp":
				default:
					return nil, fmt.Errorf("unknown protocol: %s", proto)
				}
			}
			arg = arg[:protoIdx]
		}

		var (
			localStr, remoteStr string
		)

		parts := strings.Split(arg, ":")
		switch len(parts) {
		case 1:
			localStr, remoteStr = parts[0], parts[0]
		case 2:
			localStr = parts[0]
			if len(localStr) == 0 {
				localStr = "0"
			}
			remoteStr = parts[1]
		default:
			return nil, fmt.Errorf("invalid port format: %q", arg)
		}

		localPort, err := parsePort(localStr)
		if err != nil {
			return nil, err
		}
		remotePort, err := parsePort(remoteStr)
		if err != nil {
			return nil, err
		}
		ret[i] = portPair{RemotePort: remotePort, LocalPort: localPort, Protocol: proto}
	}
	return ret, nil
}

func createStream(c httpstream.Connection, reqID string) (dataStream httpstream.Stream, errCh chan error, err error) {
	// create error stream
	headers := http.Header{}
	headers.Set(corev1.StreamType, corev1.StreamTypeError)
	headers.Set(corev1.PortHeader, strconv.Itoa(constants.ServerPort))
	headers.Set(corev1.PortForwardRequestIDHeader, reqID)
	errStream, err := c.CreateStream(headers)
	if err != nil {
		return nil, nil, fmt.Errorf("create error stream")
	}
	// we're not writing to this stream
	_ = errStream.Close()

	errCh = make(chan error)
	go func() {
		message, err := io.ReadAll(errStream)
		switch {
		case err != nil:
			errCh <- fmt.Errorf("error reading from error stream: %v", err)
		case len(message) > 0:
			errCh <- fmt.Errorf("an error occurred forwarding: %v", string(message))
		}
		close(errCh)
	}()

	// create data stream
	headers.Set(corev1.StreamType, corev1.StreamTypeData)
	dataStream, err = c.CreateStream(headers)
	if err != nil {
		return nil, nil, fmt.Errorf("create data stream: %w", err)
	}

	return dataStream, errCh, nil
}

func getProgramName() string {
	if strings.HasPrefix(filepath.Base(os.Args[0]), "kubectl-") {
		return "kubectl relay"
	}
	return "krelay"
}
