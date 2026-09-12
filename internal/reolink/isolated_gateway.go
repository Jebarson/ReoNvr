package reolink

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

func startIsolatedGateway(ctx context.Context, cliPath, gatewayPath string) (*ExecRunner, func(), error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, nil, fmt.Errorf("reserve validation gateway address: %w", err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		return nil, nil, fmt.Errorf("release validation gateway address: %w", err)
	}
	runtimeDirectory, err := os.MkdirTemp("", "reonavr-validation-")
	if err != nil {
		return nil, nil, fmt.Errorf("create validation gateway directory: %w", err)
	}

	command := exec.CommandContext(ctx, gatewayPath, "--addr", address)
	command.Env = append(os.Environ(),
		"HOME="+runtimeDirectory,
		"XDG_CACHE_HOME="+filepath.Join(runtimeDirectory, "cache"),
		"XDG_CONFIG_HOME="+filepath.Join(runtimeDirectory, "config"),
	)
	var stderr bytes.Buffer
	command.Stdout = io.Discard
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		_ = os.RemoveAll(runtimeDirectory)
		return nil, nil, errors.New("start isolated validation gateway")
	}
	done := make(chan error, 1)
	go func() {
		done <- command.Wait()
	}()
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			_ = command.Process.Kill()
			<-done
			_ = os.RemoveAll(runtimeDirectory)
		})
	}

	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		connection, dialErr := net.DialTimeout("tcp", address, 50*time.Millisecond)
		if dialErr == nil {
			_ = connection.Close()
			return &ExecRunner{Path: cliPath, GatewayAddr: address}, stop, nil
		}
		select {
		case <-ctx.Done():
			stop()
			return nil, nil, errors.New("start isolated validation gateway: timed out")
		case <-done:
			_ = os.RemoveAll(runtimeDirectory)
			return nil, nil, fmt.Errorf("start isolated validation gateway: %s", safeCommandError(stderr.String(), ""))
		case <-ticker.C:
		}
	}
}
