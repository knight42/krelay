// Package ports parses the port arguments of a forward target.
package ports

import (
	"fmt"
	"strconv"
	"strings"
)

// Pair is one local-to-remote port mapping.
type Pair struct {
	// Local is the local port to listen on. Zero means an ephemeral port.
	Local uint16
	// Remote is the destination port.
	Remote uint16
}

// Parse parses port arguments of the form [LOCAL:]REMOTE[@PROTOCOL].
// REMOTE may be a named port present in namedPorts (populated from the target
// object's spec). Only the tcp protocol is accepted.
func Parse(args []string, namedPorts map[string]uint16) ([]Pair, error) {
	ret := make([]Pair, 0, len(args))
	for _, arg := range args {
		spec := arg
		if at := strings.IndexRune(spec, '@'); at >= 0 {
			switch proto := spec[at+1:]; proto {
			case "tcp":
			case "udp":
				return nil, fmt.Errorf("UDP is not supported: %q", arg)
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

		var pair Pair
		remote, err := parsePort(remoteStr)
		if err != nil {
			port, ok := namedPorts[remoteStr]
			if !ok {
				return nil, fmt.Errorf("port name not found: %q", remoteStr)
			}
			remote = port
		}
		pair.Remote = remote

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
