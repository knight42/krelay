package ports

import (
	"fmt"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/knight42/krelay/pkg/constants"
)

type Parser struct {
	args []string
	obj  runtime.Object
}

func (p Parser) WithObject(obj runtime.Object) Parser {
	p.obj = obj
	return p
}

func (p *Parser) Parse() ([]PortPair, error) {
	var (
		allPorts portsInObject
		err      error
		ret      []PortPair
	)

	if p.obj != nil {
		allPorts, err = getPortsFromObject(p.obj)
		if err != nil {
			return nil, err
		}
	}
	for _, arg := range p.args {
		var (
			proto    string
			portPair PortPair
		)

		protoIdx := strings.IndexRune(arg, '@')
		if protoIdx > 0 {
			if protoIdx < len(arg)-1 {
				proto = arg[protoIdx+1:]
				switch proto {
				case constants.ProtocolTCP, constants.ProtocolUDP:
				default:
					return nil, fmt.Errorf("unknown protocol: %q", proto)
				}
			}
			arg = arg[:protoIdx]
		}
		portPair.Protocol = proto
		enforceProtocol := len(proto) > 0

		var (
			localStr, remoteStr string
		)
		parts := strings.Split(arg, ":")
		switch len(parts) {
		case 1:
			remoteStr = parts[0]
		case 2:
			localStr, remoteStr = parts[0], parts[1]
			if len(localStr) == 0 {
				localStr = "0"
			}
		default:
			return nil, fmt.Errorf("invalid port format: %q", arg)
		}

		// determine the remote port and protocol
		remotePort, err := parsePort(remoteStr)
		if err != nil {
			if p.obj == nil {
				return nil, err
			}
			// assume it's a name of port
			port, ok := allPorts.Names[remoteStr]
			if !ok {
				return nil, fmt.Errorf("port name not found: %q", remoteStr)
			}
			portPair.RemotePort = port.Port
			if !enforceProtocol {
				portPair.Protocol = port.Protocol
			}
		} else {
			portPair.RemotePort = remotePort
			if !enforceProtocol && p.obj != nil {
				if protos, ok := allPorts.Protocols[remotePort]; ok {
					if len(protos) > 1 {
						return nil, fmt.Errorf("ambiguous protocol of port: %d: %v", remotePort, protos)
					}
					portPair.Protocol = protos[0]
				}
			}
		}
		if len(portPair.Protocol) == 0 {
			// fallback to TCP
			portPair.Protocol = constants.ProtocolTCP
		}

		// determine the local port
		if len(localStr) == 0 {
			portPair.LocalPort = portPair.RemotePort
		} else {
			portPair.LocalPort, err = parsePort(localStr)
			if err != nil {
				return nil, err
			}
		}

		ret = append(ret, portPair)
	}
	return ret, nil
}

// NewParser creates a new parser that parse ports in args.
func NewParser(args []string) Parser {
	return Parser{args: args}
}

func parsePort(s string) (uint16, error) {
	port, err := strconv.ParseUint(s, 10, 16)
	if err != nil {
		return 0, fmt.Errorf("invalid port: %q", s)
	}
	return uint16(port), nil
}

type portsInObject struct {
	Names     map[string]portWithProtocol
	Protocols map[uint16][]string
}

type portWithProtocol struct {
	Port     uint16
	Protocol string
}

func getPortsFromObject(obj runtime.Object) (portsInObject, error) {
	switch actual := obj.(type) {
	case *corev1.Service:
		ret := portsInObject{
			Names:     map[string]portWithProtocol{},
			Protocols: map[uint16][]string{},
		}
		for _, port := range actual.Spec.Ports {
			po := uint16(port.Port)
			proto := strings.ToLower(string(port.Protocol))
			ret.Names[port.Name] = portWithProtocol{
				Port:     po,
				Protocol: proto,
			}
			ret.Protocols[po] = append(ret.Protocols[po], proto)
		}
		return ret, nil
	case *corev1.Pod:
		return getPortsFromPodSpec(&actual.Spec), nil
	case *appsv1.Deployment:
		return getPortsFromPodSpec(&actual.Spec.Template.Spec), nil
	case *appsv1.StatefulSet:
		return getPortsFromPodSpec(&actual.Spec.Template.Spec), nil
	case *appsv1.ReplicaSet:
		return getPortsFromPodSpec(&actual.Spec.Template.Spec), nil
	case *appsv1.DaemonSet:
		return getPortsFromPodSpec(&actual.Spec.Template.Spec), nil
	default:
		return portsInObject{}, fmt.Errorf("unknown object: %T", obj)
	}
}

func getPortsFromPodSpec(podSpec *corev1.PodSpec) portsInObject {
	ret := portsInObject{
		Names:     map[string]portWithProtocol{},
		Protocols: map[uint16][]string{},
	}
	for _, ct := range podSpec.Containers {
		for _, ctPort := range ct.Ports {
			po := uint16(ctPort.ContainerPort)
			proto := strings.ToLower(string(ctPort.Protocol))
			ret.Names[ctPort.Name] = portWithProtocol{
				Port:     po,
				Protocol: proto,
			}
			ret.Protocols[po] = append(ret.Protocols[po], proto)
		}
	}
	return ret
}
