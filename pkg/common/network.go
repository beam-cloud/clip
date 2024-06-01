package common

import (
	"context"
	"net"
)

func IsIPv6Available() bool {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}

	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok && ipnet.IP.To16() != nil && ipnet.IP.To4() == nil {
			return true
		}
	}
	return false
}

func DialContextIPv6(ctx context.Context, network, address string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "tcp6", address)
}
