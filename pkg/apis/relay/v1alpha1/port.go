package v1alpha1

import (
	"fmt"
	"strconv"
	"strings"
)

// PortMapping represents a local to remote port mapping.
type PortMapping struct {
	LocalPort  int
	RemotePort int
	Protocol   Protocol
}

// ParsePortMapping parses a port mapping string like "8080:80@tcp".
// Format: [local_port:]<remote_port>[@protocol]
func ParsePortMapping(s string) (PortMapping, error) {
	var pm PortMapping
	pm.Protocol = ProtocolTCP // default

	// Check for protocol suffix
	if idx := strings.LastIndex(s, "@"); idx != -1 {
		proto := strings.ToLower(s[idx+1:])
		switch proto {
		case "tcp":
			pm.Protocol = ProtocolTCP
		case "udp":
			pm.Protocol = ProtocolUDP
		default:
			return pm, fmt.Errorf("invalid protocol: %s (expected tcp or udp)", proto)
		}
		s = s[:idx]
	}

	// Check for local:remote format
	if idx := strings.Index(s, ":"); idx != -1 {
		local, err := strconv.Atoi(s[:idx])
		if err != nil {
			return pm, fmt.Errorf("invalid local port: %s", s[:idx])
		}
		remote, err := strconv.Atoi(s[idx+1:])
		if err != nil {
			return pm, fmt.Errorf("invalid remote port: %s", s[idx+1:])
		}
		pm.LocalPort = local
		pm.RemotePort = remote
	} else {
		// Only remote port specified, local defaults to same
		port, err := strconv.Atoi(s)
		if err != nil {
			return pm, fmt.Errorf("invalid port: %s", s)
		}
		pm.LocalPort = port
		pm.RemotePort = port
	}

	// Validate port ranges
	if pm.LocalPort < 1 || pm.LocalPort > 65535 {
		return pm, fmt.Errorf("local port out of range: %d", pm.LocalPort)
	}
	if pm.RemotePort < 1 || pm.RemotePort > 65535 {
		return pm, fmt.Errorf("remote port out of range: %d", pm.RemotePort)
	}

	return pm, nil
}

// String returns the string representation of the port mapping.
func (pm PortMapping) String() string {
	if pm.LocalPort == pm.RemotePort {
		if pm.Protocol == ProtocolTCP {
			return strconv.Itoa(pm.RemotePort)
		}
		return fmt.Sprintf("%d@%s", pm.RemotePort, pm.Protocol)
	}
	if pm.Protocol == ProtocolTCP {
		return fmt.Sprintf("%d:%d", pm.LocalPort, pm.RemotePort)
	}
	return fmt.Sprintf("%d:%d@%s", pm.LocalPort, pm.RemotePort, pm.Protocol)
}
