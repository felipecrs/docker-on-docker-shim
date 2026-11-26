//go:build linux

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFindCommand(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	wrapper, real, orig := t.TempDir(), t.TempDir(), t.TempDir()
	// The wrapper's own directory comes first on PATH, as when it replaces a command.
	for _, name := range []string{"foo", "docker"} {
		if err := os.Symlink(self, filepath.Join(wrapper, name)); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(real, name), "#!/bin/sh\n")
	}
	writeFile(t, filepath.Join(real, "not-executable"), "")
	if err := os.Chmod(filepath.Join(real, "not-executable"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(orig, "foo.orig"), "#!/bin/sh\n")

	tests := []struct {
		name, path, dockerPath, want string
	}{
		{name: "foo", path: wrapper + ":" + real, want: filepath.Join(real, "foo")},
		{name: "foo", path: wrapper + ":relative:" + orig + ":" + real, want: filepath.Join(orig, "foo.orig")},
		{name: "foo", path: wrapper},
		{name: "not-executable", path: real},
		{name: "./foo", path: wrapper, want: "./foo"},
		{name: "docker", path: wrapper + ":" + real, dockerPath: filepath.Join(orig, "foo.orig"), want: filepath.Join(orig, "foo.orig")},
		{name: "docker", path: wrapper + ":" + real, dockerPath: filepath.Join(wrapper, "docker")},
		{name: "docker", path: wrapper + ":" + real, dockerPath: "missing"},
		{name: "foo", path: wrapper + ":" + real, dockerPath: "missing", want: filepath.Join(real, "foo")},
	}
	for _, tt := range tests {
		t.Setenv("PATH", tt.path)
		t.Setenv(dockerPathEnv, tt.dockerPath)
		got, err := findCommand(tt.name)
		if got != tt.want || (err == nil) != (tt.want != "") {
			t.Errorf("findCommand(%q) with PATH=%s %s=%s = %q, %v, want %q", tt.name, tt.path, dockerPathEnv, tt.dockerPath, got, err, tt.want)
		}
	}
}
