package main

import (
	"testing"
)

func mnt() string { return "/mnt/" }

func TestWSLPath(t *testing.T) {
	tests := []struct{ in, want string }{
		{`C:\a b`, "/mnt/c/a b"},
		{"D:/a", "/mnt/d/a"},
		{`c:\`, "/mnt/c/"},
		{`\\wsl.localhost\Ubuntu\home\me`, "/home/me"},
		{`\\wsl$\Ubuntu\home\me`, "/home/me"},
		{`//WSL.localhost/Ubuntu`, "/"},
		{"/srv", "/srv"},
		{"cache", "cache"},
		{`\\server\share\dir`, `\\server\share\dir`},
		{"C:", "C:"},
	}
	for _, tt := range tests {
		if got := wslPath(tt.in, mnt); got != tt.want {
			t.Errorf("wslPath(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
	if got := wslPath(`E:\x`, func() string { return "/" }); got != "/e/x" {
		t.Errorf("with root /: %q", got)
	}
	if got := wslPath("/srv", func() string { t.Error("root read for a path of Linux"); return "" }); got != "/srv" {
		t.Errorf("got %q", got)
	}
}

func TestSplitBind(t *testing.T) {
	tests := []struct{ in, source, rest string }{
		{`C:\a b:/data:ro`, `C:\a b`, "/data:ro"},
		{"D:/a:/data", "D:/a", "/data"},
		{"/srv:/srv", "/srv", "/srv"},
		{"cache:/c:rw", "cache", "/c:rw"},
		{`\\wsl$\Ubuntu\x:/x`, `\\wsl$\Ubuntu\x`, "/x"},
		{"/only", "/only", ""},
	}
	for _, tt := range tests {
		if source, rest := splitBind(tt.in); source != tt.source || rest != tt.rest {
			t.Errorf("splitBind(%q) = %q, %q, want %q, %q", tt.in, source, rest, tt.source, tt.rest)
		}
	}
}

func TestAutomountRoot(t *testing.T) {
	tests := []struct{ conf, want string }{
		{"", "/mnt/"},
		{"[boot]\nsystemd = true\n", "/mnt/"},
		{"[automount]\nroot = /\n", "/"},
		{"[automount]\r\nenabled = true\r\nroot = /windir/ # drives\r\n", "/windir/"},
		{"[Automount]\nRoot=\"/x\"\n", "/x"},
		{"[network]\nroot = /no/\n[automount]\noptions = \"metadata\"\n", "/mnt/"},
		{"[automount]\n# root = /commented/\n", "/mnt/"},
	}
	for _, tt := range tests {
		if got := automountRoot(tt.conf); got != tt.want {
			t.Errorf("automountRoot(%q) = %q, want %q", tt.conf, got, tt.want)
		}
	}
}

// The wrapper of Windows maps the paths of Windows and leaves those of Linux alone.
func TestRewriteCreateWithPathsOfWindows(t *testing.T) {
	p := &proxy{wslRoot: mnt}
	body := `{"Image": "alpine", "HostConfig": {"Binds": ["C:\\a b:/data:ro", "D:/d:/d", "/srv:/srv", "cache:/c", "\\\\wsl.localhost\\Ubuntu\\home:/h"],
		"Mounts": [{"Type": "bind", "Source": "C:\\m", "Target": "/m"}, {"Type": "volume", "Source": "C:\\v", "Target": "/v"}, {"Type": "bind", "Source": "/l", "Target": "/l"}]}}`
	want := `{"Image": "alpine", "HostConfig": {"Binds": ["/mnt/c/a b:/data:ro", "/mnt/d/d:/d", "/srv:/srv", "cache:/c", "/home:/h"],
		"Mounts": [{"Type": "bind", "Source": "/mnt/c/m", "Target": "/m"}, {"Type": "volume", "Source": "C:\\v", "Target": "/v"}, {"Type": "bind", "Source": "/l", "Target": "/l"}]}}`
	got, err := rewriteCreate([]byte(body), p.mapPath)
	if err != nil || !jsonEqual(t, got, []byte(want)) {
		t.Errorf("got %s %v\nwant %s", got, err, want)
	}
}

// The wrapper of Linux leaves the paths of Windows alone.
func TestRewriteCreateLeavesPathsOfWindowsOnLinux(t *testing.T) {
	p := &proxy{inspected: true, self: &container{}}
	body := `{"HostConfig": {"Binds": ["C:\\a:/a"], "Mounts": [{"Type": "bind", "Source": "C:\\m", "Target": "/m"}]}}`
	got, err := rewriteCreate([]byte(body), p.mapPath)
	if err != nil || string(got) != body {
		t.Errorf("got %s %v", got, err)
	}
}
