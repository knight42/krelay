// Package v1alpha1 contains API types for krelay.
package v1alpha1

// Protocol represents the network protocol.
type Protocol string

const (
	ProtocolTCP Protocol = "tcp"
	ProtocolUDP Protocol = "udp"
)

// TargetType represents the type of target.
type TargetType string

const (
	TargetTypePod        TargetType = "pod"
	TargetTypeService    TargetType = "svc"
	TargetTypeDeployment TargetType = "deploy"
	TargetTypeIP         TargetType = "ip"
	TargetTypeHost       TargetType = "host"
)

// TunnelRequest represents a request to establish a tunnel.
type TunnelRequest struct {
	// TargetType is the type of target (pod, svc, deploy, ip, host).
	TargetType TargetType `json:"targetType"`

	// TargetName is the name of the target (pod name, service name, etc.).
	TargetName string `json:"targetName"`

	// Namespace is the Kubernetes namespace (for pod, svc, deploy targets).
	Namespace string `json:"namespace,omitempty"`

	// Port is the remote port to connect to.
	Port int `json:"port"`

	// Protocol is the network protocol (tcp or udp).
	Protocol Protocol `json:"protocol"`
}

// ParseTarget parses a target string like "pod/nginx" into type and name.
func ParseTarget(target string) (TargetType, string, error) {
	for _, prefix := range []TargetType{TargetTypePod, TargetTypeService, TargetTypeDeployment, TargetTypeIP, TargetTypeHost} {
		p := string(prefix) + "/"
		if len(target) > len(p) && target[:len(p)] == p {
			return prefix, target[len(p):], nil
		}
	}
	return "", "", &InvalidTargetError{Target: target}
}

// InvalidTargetError is returned when a target string is invalid.
type InvalidTargetError struct {
	Target string
}

func (e *InvalidTargetError) Error() string {
	return "invalid target: " + e.Target + " (expected format: pod/<name>, svc/<name>, deploy/<name>, ip/<addr>, or host/<hostname>)"
}
