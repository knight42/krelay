// Package resolver turns a forward target (TYPE/NAME) into the destination
// address that krelay-server should dial from inside the cluster.
package resolver

import (
	"context"
	"fmt"
	"net/netip"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/knight42/krelay/pkg/ports"
)

// Target identifies what to forward to.
type Target struct {
	Kind      string // one of ip, host, service, pod, deployment, statefulset, daemonset, replicaset
	Name      string
	Namespace string
}

func (t Target) String() string {
	return fmt.Sprintf("%s/%s", t.Kind, t.Name)
}

var kindAliases = map[string]string{
	"ip":           "ip",
	"host":         "host",
	"svc":          "service",
	"service":      "service",
	"services":     "service",
	"po":           "pod",
	"pod":          "pod",
	"pods":         "pod",
	"deploy":       "deployment",
	"deployment":   "deployment",
	"deployments":  "deployment",
	"sts":          "statefulset",
	"statefulset":  "statefulset",
	"statefulsets": "statefulset",
	"ds":           "daemonset",
	"daemonset":    "daemonset",
	"daemonsets":   "daemonset",
	"rs":           "replicaset",
	"replicaset":   "replicaset",
	"replicasets":  "replicaset",
}

// ParseTarget parses a TYPE/NAME string.
func ParseTarget(s, namespace string) (Target, error) {
	typ, name, ok := strings.Cut(s, "/")
	if !ok || name == "" || strings.Contains(name, "/") {
		return Target{}, fmt.Errorf("invalid target %q: expected TYPE/NAME", s)
	}
	kind, ok := kindAliases[strings.ToLower(typ)]
	if !ok {
		return Target{}, fmt.Errorf("unsupported resource type: %q", typ)
	}
	return Target{Kind: kind, Name: name, Namespace: namespace}, nil
}

// AddrGetter returns the destination host (IP or hostname, without port) for
// a new tunnel connection. Dynamic getters re-resolve on every call so that
// forwarding survives pod churn like rolling updates.
type AddrGetter interface {
	Get(ctx context.Context) (string, error)
}

type staticAddr string

func (s staticAddr) Get(context.Context) (string, error) { return string(s), nil }

// Resolve returns the address getter for t along with the named ports
// declared by the target object (empty for ip and host targets).
func Resolve(ctx context.Context, cs kubernetes.Interface, t Target) (AddrGetter, map[string]ports.NamedPort, error) {
	switch t.Kind {
	case "ip":
		if _, err := netip.ParseAddr(t.Name); err != nil {
			return nil, nil, fmt.Errorf("invalid IP address %q: %w", t.Name, err)
		}
		return staticAddr(t.Name), nil, nil

	case "host":
		return staticAddr(t.Name), nil, nil

	case "service":
		svc, err := cs.CoreV1().Services(t.Namespace).Get(ctx, t.Name, metav1.GetOptions{})
		if err != nil {
			return nil, nil, err
		}
		namedPorts := make(map[string]ports.NamedPort)
		for _, p := range svc.Spec.Ports {
			if p.Name != "" {
				namedPorts[p.Name] = ports.NamedPort{Port: uint16(p.Port), Proto: protoOf(p.Protocol)}
			}
		}
		// The in-cluster DNS name keeps working across endpoint changes and
		// handles headless and ExternalName services uniformly.
		return staticAddr(fmt.Sprintf("%s.%s.svc", t.Name, t.Namespace)), namedPorts, nil

	case "pod":
		pod, err := cs.CoreV1().Pods(t.Namespace).Get(ctx, t.Name, metav1.GetOptions{})
		if err != nil {
			return nil, nil, err
		}
		if pod.Status.PodIP == "" {
			return nil, nil, fmt.Errorf("pod %s/%s has no IP yet", t.Namespace, t.Name)
		}
		return staticAddr(pod.Status.PodIP), namedContainerPorts(pod.Spec), nil

	default:
		selector, podSpec, err := workloadSelector(ctx, cs, t)
		if err != nil {
			return nil, nil, err
		}
		return &selectorAddr{cs: cs, namespace: t.Namespace, selector: selector, target: t.String()},
			namedContainerPorts(podSpec), nil
	}
}

func workloadSelector(ctx context.Context, cs kubernetes.Interface, t Target) (string, corev1.PodSpec, error) {
	var (
		sel  *metav1.LabelSelector
		spec corev1.PodSpec
	)
	switch t.Kind {
	case "deployment":
		obj, err := cs.AppsV1().Deployments(t.Namespace).Get(ctx, t.Name, metav1.GetOptions{})
		if err != nil {
			return "", spec, err
		}
		sel, spec = obj.Spec.Selector, obj.Spec.Template.Spec
	case "statefulset":
		obj, err := cs.AppsV1().StatefulSets(t.Namespace).Get(ctx, t.Name, metav1.GetOptions{})
		if err != nil {
			return "", spec, err
		}
		sel, spec = obj.Spec.Selector, obj.Spec.Template.Spec
	case "daemonset":
		obj, err := cs.AppsV1().DaemonSets(t.Namespace).Get(ctx, t.Name, metav1.GetOptions{})
		if err != nil {
			return "", spec, err
		}
		sel, spec = obj.Spec.Selector, obj.Spec.Template.Spec
	case "replicaset":
		obj, err := cs.AppsV1().ReplicaSets(t.Namespace).Get(ctx, t.Name, metav1.GetOptions{})
		if err != nil {
			return "", spec, err
		}
		sel, spec = obj.Spec.Selector, obj.Spec.Template.Spec
	default:
		return "", spec, fmt.Errorf("unsupported resource type: %q", t.Kind)
	}
	parsed, err := metav1.LabelSelectorAsSelector(sel)
	if err != nil {
		return "", spec, err
	}
	return parsed.String(), spec, nil
}

func namedContainerPorts(spec corev1.PodSpec) map[string]ports.NamedPort {
	ret := make(map[string]ports.NamedPort)
	for _, c := range spec.Containers {
		for _, p := range c.Ports {
			if p.Name != "" {
				ret[p.Name] = ports.NamedPort{Port: uint16(p.ContainerPort), Proto: protoOf(p.Protocol)}
			}
		}
	}
	return ret
}

func protoOf(p corev1.Protocol) string {
	if p == corev1.ProtocolUDP {
		return ports.ProtocolUDP
	}
	return ports.ProtocolTCP
}

// selectorAddr picks a ready pod matching a workload's selector on every
// call, so new connections reach live pods even during rolling updates.
type selectorAddr struct {
	cs        kubernetes.Interface
	namespace string
	selector  string
	target    string
}

func (s *selectorAddr) Get(ctx context.Context) (string, error) {
	podList, err := s.cs.CoreV1().Pods(s.namespace).List(ctx, metav1.ListOptions{LabelSelector: s.selector})
	if err != nil {
		return "", err
	}
	var candidates []string
	for i := range podList.Items {
		pod := &podList.Items[i]
		if pod.Status.PodIP == "" || pod.DeletionTimestamp != nil {
			continue
		}
		if podReady(pod) {
			candidates = append(candidates, pod.Status.PodIP)
		}
	}
	if len(candidates) == 0 {
		return "", fmt.Errorf("no ready pod found for %s (selector %q)", s.target, s.selector)
	}
	sort.Strings(candidates)
	return candidates[0], nil
}

func podReady(pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}
