//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	dockerproxy "github.com/felipecrs/docker-on-docker-proxy/proxy"
)

// The parent container mounts /fixtures from the host at the given destinations. The first cases
// are the DOND_SHIM_PRINT_COMMAND cases that scripts/test.sh had.
func TestRewriteCreate(t *testing.T) {
	tests := []struct {
		name      string
		container string // JSON of the inspection of the container dond runs in
		request   string // HostConfig of the request
		want      string // HostConfig expected, or the start of the error
	}{
		{
			name:      "volume",
			container: `{"Mounts": [{"Type": "bind", "Source": "/fixtures", "Destination": "/wd", "RW": true}]}`,
			request:   `{"Binds": ["/wd:/wd"]}`,
			want:      `{"Binds": ["/fixtures:/wd"]}`,
		},
		{
			name:      "volume read only",
			container: `{"Mounts": [{"Type": "bind", "Source": "/fixtures", "Destination": "/wd", "RW": true}]}`,
			request:   `{"Binds": ["/wd:/wd:ro"]}`,
			want:      `{"Binds": ["/fixtures:/wd:ro"]}`,
		},
		{
			name:      "volume and mount",
			container: `{"Mounts": [{"Type": "bind", "Source": "/fixtures", "Destination": "/wd", "RW": true}]}`,
			request:   `{"Binds": ["/wd:/wd:ro"], "Mounts": [{"Type": "bind", "Source": "/wd", "Target": "/wd2", "ReadOnly": true}]}`,
			want:      `{"Binds": ["/fixtures:/wd:ro"], "Mounts": [{"Type": "bind", "Source": "/fixtures", "Target": "/wd2", "ReadOnly": true}]}`,
		},
		{
			name: "nested volume keeps read only",
			container: `{"Mounts": [
				{"Type": "bind", "Source": "/fixtures", "Destination": "/wd", "RW": true},
				{"Type": "bind", "Source": "/fixtures", "Destination": "/test", "RW": true},
				{"Type": "bind", "Source": "/fixtures/testfile", "Destination": "/test/testfile", "RW": true}]}`,
			request: `{"Binds": ["/wd:/wd:ro", "/test:/test:ro"]}`,
			want:    `{"Binds": ["/fixtures:/wd:ro", "/fixtures:/test:ro", "/fixtures/testfile:/test/testfile:ro"]}`,
		},
		{
			name: "nested tmpfs is not added",
			container: `{"Mounts": [
				{"Type": "bind", "Source": "/fixtures", "Destination": "/wd", "RW": true},
				{"Type": "bind", "Source": "/fixtures", "Destination": "/test", "RW": true},
				{"Type": "bind", "Source": "/fixtures/testfile", "Destination": "/test/testfile", "RW": true},
				{"Type": "tmpfs", "Source": "", "Destination": "/test/tmpfsdir", "RW": true}]}`,
			request: `{"Binds": ["/wd:/wd:ro", "/test:/test:ro"]}`,
			want:    `{"Binds": ["/fixtures:/wd:ro", "/fixtures:/test:ro", "/fixtures/testfile:/test/testfile:ro"]}`,
		},
		{
			name: "nested mount is a copy of the original",
			container: `{"Mounts": [
				{"Type": "bind", "Source": "/fixtures", "Destination": "/wd", "RW": true},
				{"Type": "bind", "Source": "/fixtures", "Destination": "/test", "RW": true},
				{"Type": "bind", "Source": "/fixtures/testfile", "Destination": "/test/testfile", "RW": true}]}`,
			request: `{"Mounts": [
				{"Type": "bind", "Source": "/wd", "Target": "/wd", "ReadOnly": true},
				{"Type": "bind", "Source": "/test", "Target": "/test", "ReadOnly": true, "BindOptions": {"Propagation": "rprivate"}}]}`,
			want: `{"Mounts": [
				{"Type": "bind", "Source": "/fixtures", "Target": "/wd", "ReadOnly": true},
				{"Type": "bind", "Source": "/fixtures", "Target": "/test", "ReadOnly": true, "BindOptions": {"Propagation": "rprivate"}},
				{"Type": "bind", "Source": "/fixtures/testfile", "Target": "/test/testfile", "ReadOnly": true, "BindOptions": {"Propagation": "rprivate"}}]}`,
		},
		{
			name: "nested volume when the source is from the container root",
			container: `{"Driver": "overlay2", "GraphDriver": {"Data": {"MergedDir": "/var/lib/docker/overlay2/abc/merged"}}, "Mounts": [
				{"Type": "bind", "Source": "/fixtures/testfile", "Destination": "/test/testfile", "RW": true},
				{"Type": "volume", "Source": "/var/lib/docker/volumes/v/_data", "Destination": "/test/v", "RW": true}]}`,
			request: `{"Binds": ["/test:/wd"]}`,
			want:    `{"Binds": ["/var/lib/docker/overlay2/abc/merged/test:/wd", "/fixtures/testfile:/wd/testfile", "/var/lib/docker/volumes/v/_data:/wd/v"]}`,
		},
		{
			name:      "file in a mount",
			container: `{"Mounts": [{"Type": "bind", "Source": "/fixtures", "Destination": "/wd", "RW": true}]}`,
			request:   `{"Binds": ["/wd/testfile:/wd/testfile"]}`,
			want:      `{"Binds": ["/fixtures/testfile:/wd/testfile"]}`,
		},
		{
			name: "longest destination wins",
			container: `{"Driver": "overlay2", "GraphDriver": {"Data": {"MergedDir": "/m"}}, "Mounts": [
				{"Type": "bind", "Source": "/a", "Destination": "/wd", "RW": true},
				{"Type": "volume", "Source": "/b", "Destination": "/wd/sub", "RW": true},
				{"Type": "bind", "Source": "/c", "Destination": "/w", "RW": true}]}`,
			request: `{"Binds": ["/wd/sub/x:/x", "/wdx:/y"], "Mounts": [{"Type": "bind", "Source": "/wd/s", "Target": "/z"}]}`,
			want:    `{"Binds": ["/b/x:/x", "/m/wdx:/y"], "Mounts": [{"Type": "bind", "Source": "/a/s", "Target": "/z"}]}`,
		},
		{
			name:      "read-only mount stays read-only",
			container: `{"Mounts": [{"Type": "bind", "Source": "/fixtures", "Destination": "/ro", "RW": false}]}`,
			request: `{"Binds": ["/ro:/a", "/ro/x:/b:z", "/ro:/c:ro", "/ro:/d:rw,rslave"], "Mounts": [
				{"Type": "bind", "Source": "/ro/x", "Target": "/e"},
				{"Type": "bind", "Source": "/ro", "Target": "/f", "ReadOnly": true},
				{"Type": "bind", "Source": "/ro", "Target": "/g", "ReadOnly": false}]}`,
			want: `{"Binds": ["/fixtures:/a:ro", "/fixtures/x:/b:z,ro", "/fixtures:/c:ro", "/fixtures:/d:rslave,ro"], "Mounts": [
				{"Type": "bind", "Source": "/fixtures/x", "Target": "/e", "ReadOnly": true},
				{"Type": "bind", "Source": "/fixtures", "Target": "/f", "ReadOnly": true},
				{"Type": "bind", "Source": "/fixtures", "Target": "/g", "ReadOnly": true}]}`,
		},
		{
			name: "longest destination decides read-only",
			container: `{"Mounts": [
				{"Type": "bind", "Source": "/a", "Destination": "/ro", "RW": false},
				{"Type": "volume", "Source": "/b", "Destination": "/ro/rw", "RW": true},
				{"Type": "bind", "Source": "/c", "Destination": "/rw", "RW": true},
				{"Type": "bind", "Source": "/d", "Destination": "/rw/ro", "RW": false}]}`,
			request: `{"Binds": ["/ro/rw/x:/x", "/ro/y:/y", "/rw/ro/z:/z", "/rw/w:/w"]}`,
			want:    `{"Binds": ["/b/x:/x", "/a/y:/y:ro", "/d/z:/z:ro", "/c/w:/w"]}`,
		},
		{
			name: "nested mounts keep their own read-only",
			container: `{"Mounts": [
				{"Type": "bind", "Source": "/a", "Destination": "/p", "RW": false},
				{"Type": "bind", "Source": "/b", "Destination": "/p/rw", "RW": true},
				{"Type": "bind", "Source": "/c", "Destination": "/p/ro", "RW": false},
				{"Type": "bind", "Source": "/e", "Destination": "/q", "RW": true},
				{"Type": "bind", "Source": "/f", "Destination": "/q/ro", "RW": false}]}`,
			request: `{"Binds": ["/p:/p", "/q:/q:z"], "Mounts": [{"Type": "bind", "Source": "/q", "Target": "/m"}]}`,
			want: `{"Binds": ["/a:/p:ro", "/b:/p/rw", "/c:/p/ro:ro", "/e:/q:z", "/f:/q/ro:z,ro"], "Mounts": [
				{"Type": "bind", "Source": "/e", "Target": "/m"},
				{"Type": "bind", "Source": "/f", "Target": "/m/ro", "ReadOnly": true}]}`,
		},
		{
			name: "read-only root",
			container: `{"Driver": "overlay2", "GraphDriver": {"Data": {"MergedDir": "/m"}}, "HostConfig": {"ReadonlyRootfs": true},
				"Mounts": [{"Type": "bind", "Source": "/fixtures", "Destination": "/wd", "RW": true}]}`,
			request: `{"Binds": ["/etc:/e", "/wd:/wd"]}`,
			want:    `{"Binds": ["/m/etc:/e:ro", "/fixtures:/wd"]}`,
		},
		{
			name:      "overlayfs root",
			container: `{"Id": "abc", "Driver": "overlayfs", "ResolvConfPath": "/data/docker/containers/abc/resolv.conf"}`,
			request:   `{"Binds": ["/test/only-inside-container:/x"]}`,
			want:      `{"Binds": ["/data/docker/rootfs/overlayfs/abc/test/only-inside-container:/x"]}`,
		},
		{
			name:      "names, relative paths and other mount types are left alone",
			container: `{"Driver": "vfs"}`,
			request:   `{"Binds": ["myvolume:/data", "relative:/r"], "Mounts": [{"Type": "volume", "Source": "myvolume", "Target": "/v"}, {"Type": "tmpfs", "Target": "/t"}]}`,
			want:      `{"Binds": ["myvolume:/data", "relative:/r"], "Mounts": [{"Type": "volume", "Source": "myvolume", "Target": "/v"}, {"Type": "tmpfs", "Target": "/t"}]}`,
		},
		{
			name:      "unsupported driver",
			container: `{"Driver": "vfs"}`,
			request:   `{"Binds": ["/only-inside-container:/x"]}`,
			want:      "dond: unsupported storage driver \"vfs\"",
		},
		{
			name:      "unsupported driver is fine when a mount matches",
			container: `{"Driver": "vfs", "Mounts": [{"Type": "bind", "Source": "/fixtures", "Destination": "/wd", "RW": true}]}`,
			request:   `{"Binds": ["/wd:/wd"]}`,
			want:      `{"Binds": ["/fixtures:/wd"]}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var c container
			if err := json.Unmarshal([]byte(tt.container), &c); err != nil {
				t.Fatal(err)
			}
			body := `{"Image": "busybox", "Unknown": {"Kept": [1, 2]}, "HostConfig": ` + tt.request + `}`
			got, err := rewriteCreate([]byte(body), c.hostPath)
			if strings.HasPrefix(tt.want, "dond:") {
				if err == nil || !strings.HasPrefix(err.Error(), tt.want) {
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

// The wrapper of Linux leaves the paths of Windows alone.
func TestRewriteCreateLeavesPathsOfWindowsOnLinux(t *testing.T) {
	p := &proxy{inspected: true, self: &container{}}
	body := `{"HostConfig": {"Binds": ["C:\\a:/a"], "Mounts": [{"Type": "bind", "Source": "C:\\m", "Target": "/m"}]}}`
	got, err := rewriteCreate([]byte(body), p.mapPath)
	if err != nil || string(got) != body {
		t.Errorf("got %s %v", got, err)
	}
}

// rewriteCreate sends a create body through the handler of the proxy package, and returns the body
// that reaches the daemon, or the error message that the client gets.
func rewriteCreate(body []byte, mapPath dockerproxy.MapFunc) ([]byte, error) {
	var sent []byte
	handler := dockerproxy.New("dond", roundTripper(func(r *http.Request) (*http.Response, error) {
		sent, _ = io.ReadAll(r.Body)
		return &http.Response{StatusCode: http.StatusCreated, Header: http.Header{}, Body: http.NoBody}, nil
	}), mapPath, debugf)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("POST", "/v1.51/containers/create", bytes.NewReader(body)))
	if w.Code != http.StatusCreated {
		var answer struct{ Message string }
		_ = json.Unmarshal(w.Body.Bytes(), &answer)
		return nil, errors.New(answer.Message)
	}
	return sent, nil
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

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
