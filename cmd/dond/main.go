//go:build linux

// Command dond runs a command behind a Docker API proxy that fixes the host paths of bind mounts
// when the command runs in a container that uses the host's Docker daemon (docker-on-docker).
package main

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	// proxyEnv tells a wrapper started by another wrapper that the command is proxied already.
	proxyEnv      = "DOND_SHIM_PROXY"
	dockerPathEnv = "DOND_SHIM_DOCKER_PATH"
	// renameCommand is the first argument of the shim in the helper container of the daemon.
	renameCommand = "dond-shim-rename"
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
	upstream, configDir, hostArgs := resolveUpstream(filepath.Base(name) == "docker", args)
	debugf("version %s, command %s, upstream %q", version, path, upstream)
	if upstream == "" || upstream == os.Getenv(proxyEnv) {
		return execCommand(path, name, args)
	}

	listener, proxyHost, socket, err := listenPrivate()
	if err != nil {
		fmt.Fprintln(os.Stderr, "WARNING(dond-shim): running without the proxy:", err)
		return execCommand(path, name, args)
	}
	defer listener.Close()
	go (&http.Server{Handler: newProxy(upstream, socket), ErrorLog: logger()}).Serve(listener)

	for _, h := range hostArgs {
		args[h.index] = h.prefix + proxyHost
	}
	cmd := exec.Command(path, args...)
	cmd.Args[0] = name
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = append(os.Environ(), "DOCKER_HOST="+proxyHost, proxyEnv+"="+proxyHost)
	code, err := runCommand(cmd)
	// Any command, since a script can run docker buildx through a nested wrapper, which uses this proxy.
	fixBuildx(configDir, proxyHost, upstream)
	return code, err
}

// findCommand returns the real command for name: DOND_SHIM_DOCKER_PATH for docker, else
// <name>.orig, else the next <name> on PATH that is not this binary.
func findCommand(name string) (string, error) {
	self, _ := os.Stat(selfPath())
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
	if strings.ContainsAny(name, "/"+string(filepath.Separator)) {
		return name, nil
	}
	if path, err := exec.LookPath(name + ".orig"); err == nil && !isSelf(path) {
		return path, nil
	}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		// LookPath checks that the file is executable.
		if path, err := exec.LookPath(filepath.Join(dir, name)); filepath.IsAbs(dir) && err == nil && !isSelf(path) {
			return path, nil
		}
	}
	return "", fmt.Errorf("could not find the real %s: neither %s.orig nor another %s is on PATH", name, name, name)
}
