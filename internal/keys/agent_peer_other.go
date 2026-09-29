//go:build !linux && !darwin && !windows

package keys

import "net"

// checkPeer: peer credentials are not queried on this platform; the socket lives in a private 0700 directory.
func checkPeer(net.Conn) (peerInfo, error) { return peerInfo{}, nil }
