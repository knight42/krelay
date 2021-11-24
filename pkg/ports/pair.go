package ports

// PortPair consists of localPort, remotePort and the protocol.
type PortPair struct {
	LocalPort  uint16
	RemotePort uint16
	Protocol   string
}
