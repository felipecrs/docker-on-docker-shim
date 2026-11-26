//go:build !windows

package main

import (
	"context"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"unsafe"
)

const defaultHost = "unix:///var/run/docker.sock"

// execCommand replaces this process with the command, without a proxy.
func execCommand(path, name string, args []string) (int, error) {
	return 127, syscall.Exec(path, append([]string{name}, args...), os.Environ())
}

func selfPath() string { return "/proc/self/exe" }

// listenPrivate listens on a socket in a new temporary directory, which Close removes.
func listenPrivate() (listener net.Listener, host, socket string, err error) {
	dir, err := os.MkdirTemp("", "dond-shim-")
	if err != nil {
		return nil, "", "", err
	}
	socket = filepath.Join(dir, "docker.sock")
	if listener, err = net.Listen("unix", socket); err != nil {
		_ = os.RemoveAll(dir)
		return nil, "", "", err
	}
	return dirListener{listener, dir}, "unix://" + socket, socket, nil
}

type dirListener struct {
	net.Listener
	dir string
}

func (l dirListener) Close() error {
	err := l.Listener.Close()
	_ = os.RemoveAll(l.dir)
	return err
}

func dialUpstream(ctx context.Context, network, address string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, network, address)
}

// The drives of Windows are not translated outside Windows.
func wslRoot() func() string { return nil }

// runCommand runs the command, passes signals on to it, and returns its exit code.
func runCommand(cmd *exec.Cmd) (int, error) {
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGINT, syscall.SIGQUIT)
	if err := cmd.Start(); err != nil {
		return 127, err
	}
	go func() {
		for s := range signals {
			// The terminal sent it to the whole foreground process group, the child included.
			if s != syscall.SIGTERM && inForeground() {
				continue
			}
			_ = cmd.Process.Signal(s)
		}
	}()
	// The streams are files, so Wait fails only for the exit status, read below.
	_ = cmd.Wait()
	status := cmd.ProcessState.Sys().(syscall.WaitStatus)
	if status.Signaled() {
		return 128 + int(status.Signal()), nil
	}
	return status.ExitStatus(), nil
}

// inForeground tells whether this process group is the foreground group of its terminal.
func inForeground() bool {
	tty, err := syscall.Open("/dev/tty", syscall.O_RDONLY|syscall.O_NOCTTY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return false
	}
	defer syscall.Close(tty)
	var group int32
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(tty), syscall.TIOCGPGRP, uintptr(unsafe.Pointer(&group)))
	return errno == 0 && int(group) == syscall.Getpgrp()
}
