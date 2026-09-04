// Package ports parses the port arguments of a forward target.
package ports

import (
	"fmt"
	"strconv"
	"strings"
)

// Protocol names accepted in port arguments.
const (
	ProtocolTCP = "tcp"
	ProtocolUDP = "udp"
)

// NamedPort is a port declared by name on the target object.
type NamedPort struct {
	Port  uint16
	Proto string
}

// Pair is one local-to-remote port mapping.
type Pair struct {
	// Local is the local port to listen on. Zero means an ephemeral port.
	Local uint16
	// Remote is the destination port.
	Remote uint16
	// Proto is either ProtocolTCP or ProtocolUDP.
	Proto string
}

// Parse parses port arguments of the form [LOCAL:]REMOTE[@PROTOCOL].
// REMOTE may be a named port present in namedPorts (populated from the target
// object's spec), in which case the port's declared protocol applies unless
// an explicit @PROTOCOL contradicts it. Numeric ports default to TCP.
func Parse(args []string, namedPorts map[string]NamedPort) ([]Pair, error) {
	ret := make([]Pair, 0, len(args))
	for _, arg := range args {
		spec := arg
		var explicitProto string
		if at := strings.IndexRune(spec, '@'); at >= 0 {
			switch proto := spec[at+1:]; proto {
			case ProtocolTCP, ProtocolUDP:
				explicitProto = proto
			default:
				return nil, fmt.Errorf("unknown protocol %q in %q", proto, arg)
			}
			spec = spec[:at]
		}

		var localStr, remoteStr string
		switch parts := strings.Split(spec, ":"); len(parts) {
		case 1:
			remoteStr = parts[0]
		case 2:
			localStr, remoteStr = parts[0], parts[1]
		default:
			return nil, fmt.Errorf("invalid port format: %q", arg)
		}

		pair := Pair{Proto: explicitProto}
		remote, err := parsePort(remoteStr)
		if err != nil {
			named, ok := namedPorts[remoteStr]
			if !ok {
				return nil, fmt.Errorf("port name not found: %q", remoteStr)
			}
			if explicitProto != "" && explicitProto != named.Proto {
				return nil, fmt.Errorf("port %q is %s, not %s", remoteStr, named.Proto, explicitProto)
			}
			remote, pair.Proto = named.Port, named.Proto
		}
		pair.Remote = remote
		if pair.Proto == "" {
			pair.Proto = ProtocolTCP
		}

		switch localStr {
		case "":
			// "REMOTE" with no colon forwards the same port; ":REMOTE"
			// asks for an ephemeral local port and is handled below.
			if !strings.Contains(spec, ":") {
				pair.Local = pair.Remote
			}
		default:
			pair.Local, err = parsePort(localStr)
			if err != nil {
				return nil, err
			}
		}
		ret = append(ret, pair)
	}
	return ret, nil
}

func parsePort(s string) (uint16, error) {
	port, err := strconv.ParseUint(s, 10, 16)
	if err != nil {
		return 0, fmt.Errorf("invalid port: %q", s)
	}
	return uint16(port), nil
}
