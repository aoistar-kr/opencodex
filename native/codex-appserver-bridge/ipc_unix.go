//go:build !windows

package main

import (
	"net"
	"os"
	"path/filepath"
	"strings"
)

// sun_path is 108 bytes including the terminator on both Linux and Windows.
const maxSocketPathBytes = 100

func endpointName() string {
	if p := strings.TrimSpace(os.Getenv("OCX_CODEX_BRIDGE_ENDPOINT")); p != "" {
		return p
	}
	if p := strings.TrimSpace(os.Getenv("OCX_CODEX_BRIDGE_SOCKET")); p != "" {
		return p
	}
	path := filepath.Join(stateDir(), socketName)
	if len(path) > maxSocketPathBytes {
		path = filepath.Join(os.TempDir(), "opencodex-"+socketName)
	}
	return path
}

type unixListener struct {
	net.Listener
	path string
}

func listenLocal(path string) (localListener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	_ = os.Remove(path)
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	return &unixListener{Listener: listener, path: path}, nil
}

func (l *unixListener) Accept() (localConn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return conn, nil
}

func (l *unixListener) Close() error {
	err := l.Listener.Close()
	_ = os.Remove(l.path)
	return err
}

func dialLocal(path string) (localConn, error) {
	return net.Dial("unix", path)
}
