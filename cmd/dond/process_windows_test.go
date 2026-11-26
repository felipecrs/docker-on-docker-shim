package main

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

func TestListenPrivate(t *testing.T) {
	listener, host, _, err := listenPrivate()
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if !regexp.MustCompile(`^npipe:////\./pipe/dond-shim-[0-9a-f]{32}$`).MatchString(host) {
		t.Errorf("host %q", host)
	}
	path := `\\.\pipe\` + strings.TrimPrefix(host, "npipe:////./pipe/")
	_, other, _, err := listenPrivate()
	if err != nil || other == host {
		t.Errorf("a second pipe: %q %v", other, err)
	}

	go func() {
		if conn, err := listener.Accept(); err == nil {
			defer conn.Close()
			_, _ = conn.Read(make([]byte, 1))
		}
	}()
	conn, err := winio.DialPipeContext(context.Background(), path)
	if err != nil {
		t.Fatalf("the current user cannot open the pipe: %v", err)
	}
	defer conn.Close()
	// The DACL has one entry, which gives the current user everything.
	handle := windows.Handle(conn.(interface{ Fd() uintptr }).Fd())
	sd, err := windows.GetSecurityInfo(handle, windows.SE_KERNEL_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	// Built from SDDL too, as SDDL writes some accounts, like the built-in Administrator, as "LA".
	want, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;" + user.User.Sid.String() + ")")
	if err != nil {
		t.Fatal(err)
	}
	if sd.String() != want.String() {
		t.Errorf("security descriptor %s, want %s", sd, want)
	}
}

func TestListenPipeRefusesAPipeThatExists(t *testing.T) {
	path := `\\.\pipe\dond-shim-test-` + filepath.Base(t.TempDir())
	listener, err := listenPipe(path)
	if err != nil {
		t.Fatal(err)
	}
	if !pipeExists(path) {
		t.Error("the pipe is not there")
	}
	if second, err := listenPipe(path); err == nil {
		second.Close()
		t.Error("listened on a pipe that exists")
	}
	listener.Close()
	if pipeExists(path) {
		t.Error("the pipe is still there")
	}
}

// pipeExists tells whether a named pipe exists. A pipe whose instances are busy cannot be opened.
func pipeExists(path string) bool {
	_, err := os.Stat(path)
	return !errors.Is(err, fs.ErrNotExist)
}

func TestFindCommandOnWindows(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	shim, real, orig := t.TempDir(), t.TempDir(), t.TempDir()
	// A hard link is this binary itself, as when the shim is installed as docker.exe.
	if err := os.Link(self, filepath.Join(shim, "docker.exe")); err != nil {
		t.Skip("needs a hard link:", err)
	}
	for _, path := range []string{filepath.Join(real, "docker.exe"), filepath.Join(orig, "docker.orig.exe"), filepath.Join(real, "notes.txt")} {
		if err := os.WriteFile(path, nil, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATHEXT", ".COM;.EXE;.BAT;.CMD")
	t.Setenv(dockerPathEnv, "")

	t.Setenv("PATH", shim+";"+real)
	if got, err := findCommand("docker"); !strings.EqualFold(got, filepath.Join(real, "docker.exe")) {
		t.Errorf("findCommand(docker) = %q, %v", got, err)
	}
	t.Setenv("PATH", shim+";"+orig+";"+real)
	if got, err := findCommand("docker"); !strings.EqualFold(got, filepath.Join(orig, "docker.orig.exe")) {
		t.Errorf("findCommand(docker) with docker.orig.exe = %q, %v", got, err)
	}
	t.Setenv("PATH", shim)
	if got, err := findCommand("docker"); err == nil {
		t.Errorf("found %q with only the shim on PATH", got)
	}
	t.Setenv("PATH", real)
	if got, err := findCommand("notes"); err == nil {
		t.Errorf("found %q, which is not executable", got)
	}
}

func TestDialAddressOnWindows(t *testing.T) {
	for host, want := range map[string]string{
		defaultHost:                    "wsl",
		"npipe:////./pipe/other":       "",
		"tcp://localhost:2375":         "",
		"unix:///var/run/docker.sock":  "",
		"npipe:////./pipe/dond-shim-x": "",
	} {
		if network, _ := dialAddress(host); network != want {
			t.Errorf("dialAddress(%q) = %q, want %q", host, network, want)
		}
	}
}
