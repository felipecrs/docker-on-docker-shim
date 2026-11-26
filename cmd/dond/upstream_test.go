//go:build linux

package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestResolveUpstream(t *testing.T) {
	config := t.TempDir()
	writeContext(t, config, "remote", `{"Name": "remote", "Endpoints": {"docker": {"Host": "tcp://remote:2375"}}}`)
	writeContext(t, config, "secure", `{"Name": "secure", "Endpoints": {"docker": {"Host": "tcp://secure:2376"}}}`)
	if err := os.MkdirAll(filepath.Join(config, "contexts", "tls", hash("secure"), "docker"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeContext(t, config, "ssh", `{"Name": "ssh", "Endpoints": {"docker": {"Host": "ssh://user@host"}}}`)
	other := t.TempDir()
	writeFile(t, filepath.Join(other, "config.json"), `{"currentContext": "remote"}`)
	writeContext(t, other, "remote", `{"Endpoints": {"docker": {"Host": "unix:///other.sock"}}}`)
	home := t.TempDir()
	writeFile(t, filepath.Join(home, ".docker", "config.json"), `{"currentContext": "remote"}`)
	writeContext(t, filepath.Join(home, ".docker"), "remote", `{"Endpoints": {"docker": {"Host": "unix:///home.sock"}}}`)
	empty := t.TempDir()

	tests := []struct {
		name     string
		notDock  bool
		args     string
		env      map[string]string
		want     string
		wantArgs string // args with the -H values replaced by PROXY
	}{
		{name: "default", args: "ps", want: defaultHost},
		{name: "DOCKER_HOST", args: "ps", env: map[string]string{"DOCKER_HOST": "tcp://h:1"}, want: "tcp://h:1"},
		{name: "-H x", args: "-H unix:///a ps", want: "unix:///a", wantArgs: "-H PROXY ps"},
		{name: "-H=x", args: "-H=unix:///a ps", want: "unix:///a", wantArgs: "-H=PROXY ps"},
		{name: "-Hx", args: "-Hunix:///a ps", want: "unix:///a", wantArgs: "-HPROXY ps"},
		{name: "--host x", args: "--host unix:///a ps", want: "unix:///a", wantArgs: "--host PROXY ps"},
		{name: "--host=x", args: "--host=unix:///a ps", want: "unix:///a", wantArgs: "--host=PROXY ps"},
		{name: "-H wins over DOCKER_HOST", args: "-D -H unix:///a ps", env: map[string]string{"DOCKER_HOST": "tcp://h:1"}, want: "unix:///a", wantArgs: "-D -H PROXY ps"},
		{name: "options with a value are skipped", args: "--log-level -H -l debug --host unix:///a ps", want: "unix:///a", wantArgs: "--log-level -H -l debug --host PROXY ps"},
		{name: "only before the first positional", args: "run -H unix:///a busybox", want: defaultHost},
		{name: "only for docker", notDock: true, args: "-H unix:///a", want: defaultHost},
		{name: "--context", args: "--context remote ps", want: ""},
		{name: "-c", args: "-c=remote ps", want: ""},
		{name: "--context default", args: "--context default ps", env: map[string]string{"DOCKER_CONFIG": other}, want: defaultHost},
		{name: "-c=default with DOCKER_HOST", args: "-c=default ps", env: map[string]string{"DOCKER_HOST": "tcp://h:1"}, want: "tcp://h:1"},
		{name: "--tlsverify", args: "--tlsverify -H tcp://h:2376 ps", want: ""},
		{name: "--tlscacert", args: "--tlscacert ca.pem ps", want: ""},
		{name: "DOCKER_TLS_VERIFY", args: "ps", env: map[string]string{"DOCKER_HOST": "tcp://h:2376", "DOCKER_TLS_VERIFY": "1"}, want: ""},
		{name: "ssh", args: "ps", env: map[string]string{"DOCKER_HOST": "ssh://user@host"}, want: ""},
		{name: "npipe", args: "ps", env: map[string]string{"DOCKER_HOST": "npipe:////./pipe/docker_engine"}, want: ""},
		{name: "DOCKER_CONTEXT", args: "ps", env: map[string]string{"DOCKER_CONFIG": config, "DOCKER_CONTEXT": "remote"}, want: "tcp://remote:2375"},
		{name: "DOCKER_HOST wins over DOCKER_CONTEXT", args: "ps", env: map[string]string{"DOCKER_CONFIG": config, "DOCKER_CONTEXT": "remote", "DOCKER_HOST": "unix:///a"}, want: "unix:///a"},
		{name: "default context", args: "ps", env: map[string]string{"DOCKER_CONFIG": config, "DOCKER_CONTEXT": "default"}, want: defaultHost},
		{name: "context with TLS", args: "ps", env: map[string]string{"DOCKER_CONFIG": config, "DOCKER_CONTEXT": "secure"}, want: ""},
		{name: "ssh context", args: "ps", env: map[string]string{"DOCKER_CONFIG": config, "DOCKER_CONTEXT": "ssh"}, want: ""},
		{name: "missing context", args: "ps", env: map[string]string{"DOCKER_CONFIG": config, "DOCKER_CONTEXT": "missing"}, want: ""},
		{name: "currentContext", args: "ps", env: map[string]string{"DOCKER_CONFIG": other}, want: "unix:///other.sock"},
		{name: "--config", args: "--config " + other + " ps", want: "unix:///other.sock"},
		{name: "currentContext in HOME", args: "ps", env: map[string]string{"HOME": home}, want: "unix:///home.sock"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, name := range []string{"DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_CONFIG", "DOCKER_TLS_VERIFY", "DOCKER_TLS"} {
				t.Setenv(name, "")
			}
			t.Setenv("HOME", empty)
			for name, value := range tt.env {
				t.Setenv(name, value)
			}
			args := strings.Fields(tt.args)
			got, _, hostArgs := resolveUpstream(!tt.notDock, args)
			if got != tt.want {
				t.Errorf("upstream = %q, want %q", got, tt.want)
			}
			for _, h := range hostArgs {
				args[h.index] = h.prefix + "PROXY"
			}
			if tt.wantArgs == "" {
				tt.wantArgs = tt.args
			}
			if got != "" && !reflect.DeepEqual(args, strings.Fields(tt.wantArgs)) {
				t.Errorf("args = %q, want %q", args, tt.wantArgs)
			}
		})
	}
}

func TestDialAddress(t *testing.T) {
	for host, want := range map[string]string{
		"unix:///var/run/docker.sock": "unix /var/run/docker.sock",
		"tcp://h:1":                   "tcp h:1",
		"tcp://h":                     "tcp h:2375",
		"tcp://[::1]":                 "tcp [::1]:2375",
		"tcp://h:1/base":              " ",
		"unix://":                     " ",
		"fd://":                       " ",
		"/var/run/docker.sock":        " ",
	} {
		network, address := dialAddress(host)
		if got := network + " " + address; got != want {
			t.Errorf("dialAddress(%q) = %q, want %q", host, got, want)
		}
	}
}

func hash(name string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(name)))
}

func writeContext(t *testing.T, config, name, meta string) {
	writeFile(t, filepath.Join(config, "contexts", "meta", hash(name), "meta.json"), meta)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}
