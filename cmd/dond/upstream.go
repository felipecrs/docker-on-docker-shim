package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
)

const defaultHost = "unix:///var/run/docker.sock"

// globalOptionsWithValue is the only knowledge of the docker command line the shim keeps.
var globalOptionsWithValue = map[string]bool{
	"--config": true, "-c": true, "--context": true, "-H": true, "--host": true, "-l": true,
	"--log-level": true, "--tlscacert": true, "--tlscert": true, "--tlskey": true,
}

// hostArg is a -H value in the command line: args[index] becomes prefix plus the proxy's address.
type hostArg struct {
	index  int
	prefix string
}

// resolveUpstream returns the endpoint the command would use, in the docker CLI's own order, and
// where -H values are. It returns "" when the command must run without the proxy.
func resolveUpstream(isDocker bool, args []string) (string, []hostArg) {
	host, configDir := "", os.Getenv("DOCKER_CONFIG")
	var hostArgs []hostArg
	for i := 0; isDocker && i < len(args); i++ {
		arg := args[i]
		if arg == "-" || arg == "--" || !strings.HasPrefix(arg, "-") {
			break
		}
		name, value, hasValue := strings.Cut(arg, "=")
		prefix := name + "="
		if !hasValue && !strings.HasPrefix(arg, "--") && globalOptionsWithValue[arg[:2]] {
			// -Hvalue
			name, value, hasValue, prefix = arg[:2], arg[2:], len(arg) > 2, arg[:2]
		}
		if strings.HasPrefix(name, "--tls") {
			return "", nil
		}
		if !globalOptionsWithValue[name] {
			continue
		}
		if !hasValue {
			if i++; i == len(args) {
				break
			}
			value, prefix = args[i], ""
		}
		switch name {
		case "-c", "--context":
			return "", nil
		case "-H", "--host":
			host = value
			hostArgs = append(hostArgs, hostArg{i, prefix})
		case "--config":
			configDir = value
		}
	}
	if os.Getenv("DOCKER_TLS_VERIFY") != "" || os.Getenv("DOCKER_TLS") != "" {
		return "", nil
	}
	if host == "" {
		host = os.Getenv("DOCKER_HOST")
	}
	if host == "" {
		host = contextHost(configDir)
	}
	if network, _ := dialAddress(host); network == "" {
		return "", nil
	}
	return host, hostArgs
}

// contextHost returns the endpoint of the current docker context, or "" for one the proxy does not
// support.
func contextHost(configDir string) string {
	if configDir == "" {
		configDir = filepath.Join(os.Getenv("HOME"), ".docker")
	}
	name := os.Getenv("DOCKER_CONTEXT")
	if name == "" {
		var config struct{ CurrentContext string }
		if data, err := os.ReadFile(filepath.Join(configDir, "config.json")); err == nil {
			_ = json.Unmarshal(data, &config)
		}
		name = config.CurrentContext
	}
	if name == "" || name == "default" {
		return defaultHost
	}
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(name)))
	if _, err := os.Stat(filepath.Join(configDir, "contexts", "tls", hash)); err == nil {
		return ""
	}
	var meta struct {
		Endpoints struct {
			Docker struct{ Host string } `json:"docker"`
		}
	}
	if data, err := os.ReadFile(filepath.Join(configDir, "contexts", "meta", hash, "meta.json")); err == nil {
		_ = json.Unmarshal(data, &meta)
	}
	return meta.Endpoints.Docker.Host
}

// dialAddress returns how to reach host, or "" for a host the proxy does not support.
func dialAddress(host string) (network, address string) {
	if path, ok := strings.CutPrefix(host, "unix://"); ok && path != "" {
		return "unix", path
	}
	if address, ok := strings.CutPrefix(host, "tcp://"); ok && address != "" && !strings.Contains(address, "/") {
		if _, _, err := net.SplitHostPort(address); err != nil {
			address += ":2375"
		}
		return "tcp", address
	}
	return "", ""
}
