package constants

const (
	LogFieldRequestID = "reqID"
	LogFieldDestAddr  = "dstAddr"
)

const (
	ServerName = "krelay-server"
	ServerPort = 9527
)

const (
	UDPBufferSize = 65536 + 2
	TCPBufferSize = 32768
)

const PortForwardProtocolV1Name = "portforward.k8s.io"

const (
	ProtocolTCP = "tcp"
	ProtocolUDP = "udp"
)
