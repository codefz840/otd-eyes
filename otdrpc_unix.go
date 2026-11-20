//go:build !windows

package main

import (
	"context"
	"io"
	"net"
	"os"
)

func defaultOtdEndpoint() string { return "/tmp/CoreFxPipe_" + otdPipeName }

func otdEndpointExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func dialOtd(ctx context.Context, path string) (io.ReadWriteCloser, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unix", path)
}
