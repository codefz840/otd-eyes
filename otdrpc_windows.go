//go:build windows

package main

import (
	"context"
	"io"
	"os"
	"strings"

	"github.com/Microsoft/go-winio"
)

func defaultOtdEndpoint() string { return `\\.\pipe\` + otdPipeName }

// os.Stat would open (and use up) a pipe instance; list the pipe namespace instead.
func otdEndpointExists(path string) bool {
	name := strings.TrimPrefix(path, `\\.\pipe\`)
	entries, err := os.ReadDir(`\\.\pipe\`)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if strings.EqualFold(e.Name(), name) {
			return true
		}
	}
	return false
}

// go-winio opens the pipe in overlapped mode: reading and writing at the same
// time would block each other on a plain synchronous handle.
func dialOtd(ctx context.Context, path string) (io.ReadWriteCloser, error) {
	return winio.DialPipeContext(ctx, path)
}
