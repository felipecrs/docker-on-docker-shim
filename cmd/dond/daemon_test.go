package main

import (
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestStartState(t *testing.T) {
	dir := t.TempDir()
	live, dead := filepath.Join(dir, "live.sock"), filepath.Join(dir, "dead.sock")
	listener, err := net.Listen("unix", live)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	// A socket nobody listens on, as left by a daemon that was killed.
	fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Bind(fd, &syscall.SockaddrUnix{Name: dead}); err != nil {
		t.Fatal(err)
	}
	syscall.Close(fd)
	file := filepath.Join(dir, "file")
	writeFile(t, file, "")
	link := func(name, target string) string {
		path := filepath.Join(dir, name)
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
		return path
	}
	toLive, toLiveRelative, toOther := link("to-live", live), link("to-live-relative", "live.sock"), link("to-other", file)
	missing := filepath.Join(dir, "missing")
	mountinfo := func(path string) []byte {
		return []byte("1 2 0:1 / / rw - overlay overlay rw\n3 1 0:2 /docker.sock " + path + " rw - tmpfs tmpfs rw\n")
	}

	for _, tt := range []struct {
		path, hostSock string
		mountinfo      []byte
		want           string
	}{
		{path: file, hostSock: missing, mountinfo: mountinfo(file), want: "mount"},
		// After a kill and a container restart, the real socket is mounted over the dead one.
		{path: dead, hostSock: file, mountinfo: mountinfo(dead), want: "mount"},
		{path: toLive, hostSock: live, mountinfo: mountinfo("/elsewhere"), want: "symlink"},
		{path: toLiveRelative, hostSock: live, want: "symlink"},
		{path: dead, hostSock: live, want: "dead socket"},
		{path: missing, hostSock: live, want: "nothing"},
		{path: toOther, hostSock: live},
		{path: live, hostSock: live},
		{path: file, hostSock: live},
		{path: toLive, hostSock: dead},
		{path: missing, hostSock: missing},
	} {
		got, err := startState(tt.path, tt.hostSock, tt.mountinfo)
		if got != tt.want || (err == nil) != (tt.want != "") {
			t.Errorf("startState(%s, %s) = %q, %v, want %q", filepath.Base(tt.path), filepath.Base(tt.hostSock), got, err, tt.want)
		}
	}
}
