package proxy

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func debugf(format string, args ...any) {}

// testMap maps /ro... read-only, adds a read-write and a read-only mount below /p, fails for /fail,
// and moves everything else below /host.
func testMap(source string) (string, bool, []NestedMount, error) {
	switch {
	case source == "/fail":
		return "", false, nil, errors.New("test: cannot map /fail")
	case source == "/p":
		return "/host/p", false, []NestedMount{{Source: "/host/rw", Rel: "rw"}, {Source: "/host/ro", Rel: "ro", ReadOnly: true}}, nil
	case strings.HasPrefix(source, "/ro"):
		return "/host" + source, true, nil, nil
	}
	return "/host" + source, false, nil, nil
}

func TestRewriteCreate(t *testing.T) {
	tests := []struct {
		name    string
		request string // HostConfig of the request
		want    string // HostConfig expected, or the error
	}{
		{
			name:    "binds",
			request: `{"Binds": ["/a:/a", "/b:/b:ro", "/p:/p:z", "v:/v", "relative:/r", "/no-destination"]}`,
			want:    `{"Binds": ["/host/a:/a", "/host/b:/b:ro", "/host/p:/p:z", "/host/rw:/p/rw:z", "/host/ro:/p/ro:z,ro", "v:/v", "relative:/r", "/no-destination"]}`,
		},
		{
			name:    "read-only binds",
			request: `{"Binds": ["/ro:/a", "/ro/x:/b:z", "/ro:/c:ro", "/ro:/d:rw,rslave"]}`,
			want:    `{"Binds": ["/host/ro:/a:ro", "/host/ro/x:/b:z,ro", "/host/ro:/c:ro", "/host/ro:/d:rslave,ro"]}`,
		},
		{
			name: "mounts",
			request: `{"Mounts": [
				{"Type": "bind", "Source": "/a", "Target": "/a", "BindOptions": {"Propagation": "rprivate"}},
				{"Type": "bind", "Source": "/ro", "Target": "/b"},
				{"Type": "bind", "Source": "/ro", "Target": "/c", "ReadOnly": false},
				{"Type": "bind", "Source": "/p", "Target": "/p"},
				{"Type": "volume", "Source": "v", "Target": "/v"},
				{"Type": "tmpfs", "Target": "/t"}]}`,
			want: `{"Mounts": [
				{"Type": "bind", "Source": "/host/a", "Target": "/a", "BindOptions": {"Propagation": "rprivate"}},
				{"Type": "bind", "Source": "/host/ro", "Target": "/b", "ReadOnly": true},
				{"Type": "bind", "Source": "/host/ro", "Target": "/c", "ReadOnly": true},
				{"Type": "bind", "Source": "/host/p", "Target": "/p"},
				{"Type": "bind", "Source": "/host/rw", "Target": "/p/rw"},
				{"Type": "bind", "Source": "/host/ro", "Target": "/p/ro", "ReadOnly": true},
				{"Type": "volume", "Source": "v", "Target": "/v"},
				{"Type": "tmpfs", "Target": "/t"}]}`,
		},
		{
			name:    "error of the mapping",
			request: `{"Binds": ["/a:/a", "/fail:/x"]}`,
			want:    "test: cannot map /fail",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := `{"Image": "busybox", "Unknown": {"Kept": [1, 2]}, "HostConfig": ` + tt.request + `}`
			got, err := rewriteCreate([]byte(body), testMap, debugf)
			if strings.HasPrefix(tt.want, "test:") {
				if err == nil || err.Error() != tt.want {
					t.Fatalf("error = %v, want %s", err, tt.want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := `{"Image": "busybox", "Unknown": {"Kept": [1, 2]}, "HostConfig": ` + tt.want + `}`
			if !jsonEqual(t, got, []byte(want)) {
				t.Errorf("got  %s\nwant %s", got, want)
			}
		})
	}
}

func TestRewriteCreateKeepsBodyWithNothingToMap(t *testing.T) {
	for _, body := range []string{
		`{"Image":"busybox",  "HostConfig": {"Binds": ["v:/v"], "Unknown": true}}`,
		`{"Image":"busybox",  "HostConfig": {"Binds": ["/host/a:/a"]}}`,
		`{"Image": "busybox"}`,
		`{"HostConfig": null}`,
		`not json`,
	} {
		got, err := rewriteCreate([]byte(body), func(source string) (string, bool, []NestedMount, error) {
			return source, false, nil, nil
		}, debugf)
		if err != nil || string(got) != body {
			t.Errorf("rewriteCreate(%s) = %s, %v", body, got, err)
		}
	}
}

// docker-wsl-proxy maps the paths of Windows and leaves those of Linux alone.
func TestRewriteCreateWithPathsOfWindows(t *testing.T) {
	wsl := map[string]string{`C:\a b`: "/mnt/c/a b", "D:/d": "/mnt/d/d", `\\wsl.localhost\Ubuntu\home`: "/home", `C:\m`: "/mnt/c/m"}
	mapPath := func(source string) (string, bool, []NestedMount, error) {
		if mapped, ok := wsl[source]; ok {
			return mapped, false, nil, nil
		}
		return source, false, nil, nil
	}
	body := `{"Image": "alpine", "HostConfig": {"Binds": ["C:\\a b:/data:ro", "D:/d:/d", "/srv:/srv", "cache:/c", "\\\\wsl.localhost\\Ubuntu\\home:/h"],
		"Mounts": [{"Type": "bind", "Source": "C:\\m", "Target": "/m"}, {"Type": "volume", "Source": "C:\\v", "Target": "/v"}, {"Type": "bind", "Source": "/l", "Target": "/l"}]}}`
	want := `{"Image": "alpine", "HostConfig": {"Binds": ["/mnt/c/a b:/data:ro", "/mnt/d/d:/d", "/srv:/srv", "cache:/c", "/home:/h"],
		"Mounts": [{"Type": "bind", "Source": "/mnt/c/m", "Target": "/m"}, {"Type": "volume", "Source": "C:\\v", "Target": "/v"}, {"Type": "bind", "Source": "/l", "Target": "/l"}]}}`
	got, err := rewriteCreate([]byte(body), mapPath, debugf)
	if err != nil || !jsonEqual(t, got, []byte(want)) {
		t.Errorf("got %s %v\nwant %s", got, err, want)
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

func TestIsPath(t *testing.T) {
	for in, want := range map[string]bool{
		"/srv": true, `C:\a`: true, "d:/a": true, `\\wsl$\Ubuntu\x`: true, `//WSL.localhost/Ubuntu`: true,
		"cache": false, "C:": false, `\\server\share\dir`: false, "relative/dir": false,
	} {
		if got := isPath(in); got != want {
			t.Errorf("isPath(%q) = %v, want %v", in, got, want)
		}
	}
}

func jsonEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &y); err != nil {
		t.Fatal(err)
	}
	return reflect.DeepEqual(x, y)
}
