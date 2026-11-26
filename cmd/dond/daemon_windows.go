package main

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
)

// enginePipe is where Windows clients of Docker look by default.
const enginePipe = `\\.\pipe\docker_engine`

// daemon serves the proxy at the default pipe of Docker until it is stopped, so that clients of
// Windows reach the daemon in WSL with no wrapper.
func daemon() (int, error) {
	listener, err := listenPipe(enginePipe)
	if err != nil {
		return 1, fmt.Errorf("cannot serve at %s, which Docker Desktop or another dond daemon may have: %w", enginePipe, err)
	}
	p := newProxy(defaultHost, "")
	wslTransport()
	fmt.Fprintf(os.Stderr, "dond-shim %s: serving at %s, forwarding to the Docker daemon in WSL\n", version, enginePipe)
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	served := make(chan error, 1)
	go func() { served <- (&http.Server{Handler: p, ErrorLog: logger()}).Serve(listener) }()
	select {
	case <-signals:
		return 0, listener.Close()
	case err := <-served:
		return 1, err
	}
}

// rename is the helper of the daemon of Linux.
func rename(string, string) (int, error) {
	return 1, errors.ErrUnsupported
}
