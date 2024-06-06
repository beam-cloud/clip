package common

import (
	"bytes"
	"context"
	"net"
	"os/exec"
)

func IsIPv6Available() bool {
	cmd := exec.Command("ping", "-6", "-c", "1", "google.com")
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	return err == nil
}

func DialContextIPv6(ctx context.Context, network, address string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "tcp6", address)
}
