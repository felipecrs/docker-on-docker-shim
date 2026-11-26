// Command dond runs a command behind a Docker API proxy that fixes the host paths of bind mounts
// when the command runs in a container that uses the host's Docker daemon (docker-on-docker).
package main

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

const (
	// proxyEnv tells a wrapper started by another wrapper that the command is proxied already.
	proxyEnv      = "DOND_SHIM_PROXY"
	dockerPathEnv = "DOND_SHIM_DOCKER_PATH"
)

// version is set at build time.
var version = "dev"

var debug = os.Getenv("DOND_SHIM_DEBUG") == "true"

func debugf(format string, args ...any) {
	if debug {
		fmt.Fprintf(os.Stderr, "DEBUG(dond-shim): "+format+"\n", args...)
	}
}

func main() {
	code, err := run()
	if err != nil {
		fmt.Fprintln(os.Stderr, "ERROR(dond-shim):", err)
	}
	os.Exit(code)
}

func run() (int, error) {
	if len(os.Args) == 4 && os.Args[1] == renameCommand {
		return rename(os.Args[2], os.Args[3])
	}
	name, args := filepath.Base(os.Args[0]), os.Args[1:]
	if name == "dond" {
		if len(args) > 0 && args[0] == "wrap" {
			if len(args) < 2 {
				return 1, errors.New("usage: dond wrap <command> [args...]")
			}
			name, args = args[1], args[2:]
		} else if len(args) > 0 && args[0] == "daemon" {
			if len(args) > 1 {
				return 1, errors.New("usage: dond daemon")
			}
			return daemon()
		} else {
			name = "docker"
		}
	}
	path, err := findCommand(name)
	if err != nil {
		return 1, err
	}
	if os.Getenv("DOND_SHIM_SKIP") == "true" {
		return execCommand(path, name, args)
	}
	upstream, hostArgs := resolveUpstream(filepath.Base(name) == "docker", args)
	debugf("version %s, command %s, upstream %q", version, path, upstream)
	if upstream == "" || upstream == os.Getenv(proxyEnv) {
		return execCommand(path, name, args)
	}

	dir, err := os.MkdirTemp("", "dond-shim-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "WARNING(dond-shim): running without the proxy:", err)
		return execCommand(path, name, args)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "docker.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		fmt.Fprintln(os.Stderr, "WARNING(dond-shim): running without the proxy:", err)
		_ = os.RemoveAll(dir)
		return execCommand(path, name, args)
	}
	defer listener.Close()
	go (&http.Server{Handler: newProxy(upstream, socket), ErrorLog: logger()}).Serve(listener)

	proxyHost := "unix://" + socket
	for _, h := range hostArgs {
		args[h.index] = h.prefix + proxyHost
	}
	cmd := exec.Command(path, args...)
	cmd.Args[0] = name
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = append(os.Environ(), "DOCKER_HOST="+proxyHost, proxyEnv+"="+proxyHost)

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

// execCommand replaces this process with the command, without a proxy.
func execCommand(path, name string, args []string) (int, error) {
	return 127, syscall.Exec(path, append([]string{name}, args...), os.Environ())
}

// findCommand returns the real command for name: DOND_SHIM_DOCKER_PATH for docker, else
// <name>.orig, else the next <name> on PATH that is not this binary.
func findCommand(name string) (string, error) {
	self, _ := os.Stat("/proc/self/exe")
	isSelf := func(path string) bool {
		info, err := os.Stat(path)
		return err == nil && os.SameFile(info, self)
	}
	if custom := os.Getenv(dockerPathEnv); custom != "" && name == "docker" {
		path, err := exec.LookPath(custom)
		if err != nil {
			return "", fmt.Errorf("%s (%s) points to a non-existing file or command", dockerPathEnv, custom)
		}
		if isSelf(path) {
			return "", fmt.Errorf("%s (%s) points to this shim", dockerPathEnv, custom)
		}
		return path, nil
	}
	if strings.Contains(name, "/") {
		return name, nil
	}
	if path, err := exec.LookPath(name + ".orig"); err == nil && !isSelf(path) {
		return path, nil
	}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		path := filepath.Join(dir, name)
		info, err := os.Stat(path)
		if filepath.IsAbs(dir) && err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 && !os.SameFile(info, self) {
			return path, nil
		}
	}
	return "", fmt.Errorf("could not find the real %s: neither %s.orig nor another %s is on PATH", name, name, name)
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
