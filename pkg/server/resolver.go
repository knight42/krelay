package server

import (
	"context"
	"fmt"
	"net"
	"sync"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"

	"github.com/knight/krelay/pkg/apis/relay/v1alpha1"
)

// Resolver resolves targets to network addresses.
type Resolver struct {
	client kubernetes.Interface
}

// NewResolver creates a new Resolver.
func NewResolver(client kubernetes.Interface) *Resolver {
	return &Resolver{client: client}
}

// ResolvedTarget contains the resolved address and optional watch for auto-reconnect.
type ResolvedTarget struct {
	// Address is the resolved network address (ip:port).
	Address string

	// Watcher is set for deployment targets to watch for pod deletion.
	// When the current pod is deleted, a new address will be sent on the channel.
	Watcher <-chan string

	// StopWatch should be called to stop watching.
	StopWatch func()
}

// Resolve resolves a target to a network address.
func (r *Resolver) Resolve(ctx context.Context, req *v1alpha1.TunnelRequest) (*ResolvedTarget, error) {
	switch req.TargetType {
	case v1alpha1.TargetTypePod:
		return r.resolvePod(ctx, req)
	case v1alpha1.TargetTypeService:
		return r.resolveService(ctx, req)
	case v1alpha1.TargetTypeDeployment:
		return r.resolveDeployment(ctx, req)
	case v1alpha1.TargetTypeIP:
		return r.resolveIP(req)
	case v1alpha1.TargetTypeHost:
		return r.resolveHost(req)
	default:
		return nil, fmt.Errorf("unknown target type: %s", req.TargetType)
	}
}

func (r *Resolver) resolvePod(ctx context.Context, req *v1alpha1.TunnelRequest) (*ResolvedTarget, error) {
	pod, err := r.client.CoreV1().Pods(req.Namespace).Get(ctx, req.TargetName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get pod %s/%s: %w", req.Namespace, req.TargetName, err)
	}

	if pod.Status.PodIP == "" {
		return nil, fmt.Errorf("pod %s/%s has no IP address", req.Namespace, req.TargetName)
	}

	return &ResolvedTarget{
		Address:   net.JoinHostPort(pod.Status.PodIP, fmt.Sprintf("%d", req.Port)),
		StopWatch: func() {},
	}, nil
}

func (r *Resolver) resolveService(ctx context.Context, req *v1alpha1.TunnelRequest) (*ResolvedTarget, error) {
	svc, err := r.client.CoreV1().Services(req.Namespace).Get(ctx, req.TargetName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get service %s/%s: %w", req.Namespace, req.TargetName, err)
	}

	// Use the service's cluster DNS name for resilience to rolling updates
	// Format: <service-name>.<namespace>.svc.cluster.local
	dnsName := fmt.Sprintf("%s.%s.svc.cluster.local", svc.Name, svc.Namespace)

	return &ResolvedTarget{
		Address:   net.JoinHostPort(dnsName, fmt.Sprintf("%d", req.Port)),
		StopWatch: func() {},
	}, nil
}

func (r *Resolver) resolveDeployment(ctx context.Context, req *v1alpha1.TunnelRequest) (*ResolvedTarget, error) {
	deploy, err := r.client.AppsV1().Deployments(req.Namespace).Get(ctx, req.TargetName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get deployment %s/%s: %w", req.Namespace, req.TargetName, err)
	}

	// Find a ready pod for this deployment
	podIP, podName, err := r.findDeploymentPod(ctx, deploy)
	if err != nil {
		return nil, err
	}

	// Set up watcher for auto-reconnect
	addrCh := make(chan string, 1)
	stopCh := make(chan struct{})
	var once sync.Once

	go r.watchDeploymentPods(ctx, deploy, podName, req.Port, addrCh, stopCh)

	return &ResolvedTarget{
		Address: net.JoinHostPort(podIP, fmt.Sprintf("%d", req.Port)),
		Watcher: addrCh,
		StopWatch: func() {
			once.Do(func() {
				close(stopCh)
			})
		},
	}, nil
}

func (r *Resolver) findDeploymentPod(ctx context.Context, deploy *appsv1.Deployment) (string, string, error) {
	selector, err := metav1.LabelSelectorAsSelector(deploy.Spec.Selector)
	if err != nil {
		return "", "", fmt.Errorf("invalid selector: %w", err)
	}

	pods, err := r.client.CoreV1().Pods(deploy.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector.String(),
	})
	if err != nil {
		return "", "", fmt.Errorf("failed to list pods: %w", err)
	}

	for _, pod := range pods.Items {
		if pod.Status.Phase == corev1.PodRunning && pod.Status.PodIP != "" {
			// Check if pod is ready
			for _, cond := range pod.Status.Conditions {
				if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
					return pod.Status.PodIP, pod.Name, nil
				}
			}
		}
	}

	return "", "", fmt.Errorf("no ready pods found for deployment %s/%s", deploy.Namespace, deploy.Name)
}

func (r *Resolver) watchDeploymentPods(ctx context.Context, deploy *appsv1.Deployment, currentPod string, port int, addrCh chan<- string, stopCh <-chan struct{}) {
	defer close(addrCh)

	selector, err := metav1.LabelSelectorAsSelector(deploy.Spec.Selector)
	if err != nil {
		return
	}

	watcher, err := r.client.CoreV1().Pods(deploy.Namespace).Watch(ctx, metav1.ListOptions{
		LabelSelector: selector.String(),
	})
	if err != nil {
		return
	}
	defer watcher.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-stopCh:
			return
		case event, ok := <-watcher.ResultChan():
			if !ok {
				return
			}

			if event.Type == watch.Deleted {
				pod, ok := event.Object.(*corev1.Pod)
				if !ok {
					continue
				}

				// Check if deleted pod is our current pod
				if pod.Name == currentPod {
					// Find a new pod
					newIP, newName, err := r.findDeploymentPod(ctx, deploy)
					if err != nil {
						continue
					}
					currentPod = newName

					select {
					case addrCh <- net.JoinHostPort(newIP, fmt.Sprintf("%d", port)):
					case <-stopCh:
						return
					case <-ctx.Done():
						return
					}
				}
			}
		}
	}
}

func (r *Resolver) resolveIP(req *v1alpha1.TunnelRequest) (*ResolvedTarget, error) {
	ip := net.ParseIP(req.TargetName)
	if ip == nil {
		return nil, fmt.Errorf("invalid IP address: %s", req.TargetName)
	}

	return &ResolvedTarget{
		Address:   net.JoinHostPort(req.TargetName, fmt.Sprintf("%d", req.Port)),
		StopWatch: func() {},
	}, nil
}

func (r *Resolver) resolveHost(req *v1alpha1.TunnelRequest) (*ResolvedTarget, error) {
	// For host targets, we use the hostname directly and let the server resolve it
	return &ResolvedTarget{
		Address:   net.JoinHostPort(req.TargetName, fmt.Sprintf("%d", req.Port)),
		StopWatch: func() {},
	}, nil
}

// IsPodSelector checks if the given labels match the deployment's selector.
func IsPodSelector(selector *metav1.LabelSelector, podLabels map[string]string) bool {
	s, err := metav1.LabelSelectorAsSelector(selector)
	if err != nil {
		return false
	}
	return s.Matches(labels.Set(podLabels))
}
