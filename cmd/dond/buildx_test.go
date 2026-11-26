package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestFixBuildx(t *testing.T) {
	const proxy = "unix:///tmp/dond-shim-1/docker.sock"
	instance := func(endpoint string) string {
		return `{"Name":"x","Driver":"docker-container","Nodes":[{"Name":"x0","Endpoint":"` + endpoint + `","Platforms":null}],"Dynamic":false}`
	}
	tests := []struct {
		name, upstream, current, wantEndpoint string
		buildxConfig, wantGlobal              bool
	}{
		{name: "default endpoint", upstream: defaultHost, current: proxy, wantEndpoint: "default", wantGlobal: true},
		{name: "other endpoint", upstream: "tcp://remote:2375", current: proxy, wantEndpoint: "tcp://remote:2375", wantGlobal: true},
		{name: "BUILDX_CONFIG", upstream: defaultHost, current: proxy, wantEndpoint: "default", wantGlobal: true, buildxConfig: true},
		{name: "current of another endpoint", upstream: defaultHost, current: "unix:///other.sock", wantEndpoint: "default"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			configDir := t.TempDir()
			dir := filepath.Join(configDir, "buildx")
			t.Setenv("BUILDX_CONFIG", "")
			if tt.buildxConfig {
				dir = t.TempDir()
				t.Setenv("BUILDX_CONFIG", dir)
			}
			if err := os.MkdirAll(filepath.Join(dir, "instances"), 0o700); err != nil {
				t.Fatal(err)
			}
			write := func(name, content string) {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			write("instances/mine", instance(proxy))
			write("instances/other", instance("unix:///tmp/dond-shim-2/docker.sock"))
			current := `{"Key":"` + tt.current + `","Name":"mine","Global":false}`
			write("current", current)

			fixBuildx(configDir, proxy, tt.upstream)

			read := func(name string) string {
				data, err := os.ReadFile(filepath.Join(dir, name))
				if err != nil {
					t.Fatal(err)
				}
				return string(data)
			}
			if got, want := read("instances/mine"), instance(tt.wantEndpoint); got != want {
				t.Errorf("mine = %s, want %s", got, want)
			}
			if got, want := read("instances/other"), instance("unix:///tmp/dond-shim-2/docker.sock"); got != want {
				t.Errorf("other = %s, want %s", got, want)
			}
			var got struct {
				Key, Name string
				Global    bool
			}
			if err := json.Unmarshal([]byte(read("current")), &got); err != nil || got.Key != tt.current || got.Name != "mine" || got.Global != tt.wantGlobal {
				t.Errorf("current = %+v %v, want Global %v", got, err, tt.wantGlobal)
			}
			if entries, _ := os.ReadDir(filepath.Join(dir, "instances")); len(entries) != 2 {
				t.Errorf("instances has %d files, want 2", len(entries))
			}
		})
	}
	t.Run("no buildx directory", func(t *testing.T) {
		t.Setenv("BUILDX_CONFIG", "")
		configDir := t.TempDir()
		fixBuildx(configDir, proxy, defaultHost)
		if _, err := os.Stat(filepath.Join(configDir, "buildx")); !os.IsNotExist(err) {
			t.Errorf("buildx directory: %v", err)
		}
	})
}
