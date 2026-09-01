// Package constants holds values shared between the krelay client and server.
package constants

import "time"

const (
	// ServerName is the base name of the in-cluster server workload.
	ServerName = "krelay-server"

	// TunnelPort is the TCP port on the tailcat server's virtual address
	// that carries multiplexed krelay tunnel connections.
	TunnelPort uint16 = 9527

	// TokenPrefix marks the stdout line of krelay-server that carries the
	// tailcat connection blob. The client scans pod logs for this prefix.
	TokenPrefix = "KRELAY_TOKEN="

	// DefaultIdleTimeout is how long krelay-server keeps running with no
	// active tunnel connections before exiting. It is a safety net for
	// clients that die without deleting the server Job; a live client
	// always holds a heartbeat connection open.
	DefaultIdleTimeout = 10 * time.Minute
)
