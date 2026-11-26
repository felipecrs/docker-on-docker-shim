package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// The parent container mounts /fixtures from the host at the given destinations. The first cases
// are the DOND_SHIM_PRINT_COMMAND cases that scripts/test.sh had.
func TestRewriteCreate(t *testing.T) {
	tests := []struct {
		name      string
		container string // JSON of the inspection of the container the shim runs in
		request   string // HostConfig of the request
		want      string // HostConfig expected, or the start of the error
	}{
		{
			name:      "volume",
			container: `{"Mounts": [{"Type": "bind", "Source": "/fixtures", "Destination": "/wd"}]}`,
			request:   `{"Binds": ["/wd:/wd"]}`,
			want:      `{"Binds": ["/fixtures:/wd"]}`,
		},
		{
			name:      "volume read only",
			container: `{"Mounts": [{"Type": "bind", "Source": "/fixtures", "Destination": "/wd"}]}`,
			request:   `{"Binds": ["/wd:/wd:ro"]}`,
			want:      `{"Binds": ["/fixtures:/wd:ro"]}`,
		},
		{
			name:      "volume and mount",
			container: `{"Mounts": [{"Type": "bind", "Source": "/fixtures", "Destination": "/wd"}]}`,
			request:   `{"Binds": ["/wd:/wd:ro"], "Mounts": [{"Type": "bind", "Source": "/wd", "Target": "/wd2", "ReadOnly": true}]}`,
			want:      `{"Binds": ["/fixtures:/wd:ro"], "Mounts": [{"Type": "bind", "Source": "/fixtures", "Target": "/wd2", "ReadOnly": true}]}`,
		},
		{
			name: "nested volume keeps read only",
			container: `{"Mounts": [
				{"Type": "bind", "Source": "/fixtures", "Destination": "/wd"},
				{"Type": "bind", "Source": "/fixtures", "Destination": "/test"},
				{"Type": "bind", "Source": "/fixtures/testfile", "Destination": "/test/testfile"}]}`,
			request: `{"Binds": ["/wd:/wd:ro", "/test:/test:ro"]}`,
			want:    `{"Binds": ["/fixtures:/wd:ro", "/fixtures:/test:ro", "/fixtures/testfile:/test/testfile:ro"]}`,
		},
		{
			name: "nested tmpfs is not added",
			container: `{"Mounts": [
				{"Type": "bind", "Source": "/fixtures", "Destination": "/wd"},
				{"Type": "bind", "Source": "/fixtures", "Destination": "/test"},
				{"Type": "bind", "Source": "/fixtures/testfile", "Destination": "/test/testfile"},
				{"Type": "tmpfs", "Source": "", "Destination": "/test/tmpfsdir"}]}`,
			request: `{"Binds": ["/wd:/wd:ro", "/test:/test:ro"]}`,
			want:    `{"Binds": ["/fixtures:/wd:ro", "/fixtures:/test:ro", "/fixtures/testfile:/test/testfile:ro"]}`,
		},
		{
			name: "nested mount is a copy of the original",
			container: `{"Mounts": [
				{"Type": "bind", "Source": "/fixtures", "Destination": "/wd"},
				{"Type": "bind", "Source": "/fixtures", "Destination": "/test"},
				{"Type": "bind", "Source": "/fixtures/testfile", "Destination": "/test/testfile"}]}`,
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
				{"Type": "bind", "Source": "/fixtures/testfile", "Destination": "/test/testfile"},
				{"Type": "volume", "Source": "/var/lib/docker/volumes/v/_data", "Destination": "/test/v"}]}`,
			request: `{"Binds": ["/test:/wd"]}`,
			want:    `{"Binds": ["/var/lib/docker/overlay2/abc/merged/test:/wd", "/fixtures/testfile:/wd/testfile", "/var/lib/docker/volumes/v/_data:/wd/v"]}`,
		},
		{
			name:      "file in a mount",
			container: `{"Mounts": [{"Type": "bind", "Source": "/fixtures", "Destination": "/wd"}]}`,
			request:   `{"Binds": ["/wd/testfile:/wd/testfile"]}`,
			want:      `{"Binds": ["/fixtures/testfile:/wd/testfile"]}`,
		},
		{
			name: "longest destination wins",
			container: `{"Driver": "overlay2", "GraphDriver": {"Data": {"MergedDir": "/m"}}, "Mounts": [
				{"Type": "bind", "Source": "/a", "Destination": "/wd"},
				{"Type": "volume", "Source": "/b", "Destination": "/wd/sub"},
				{"Type": "bind", "Source": "/c", "Destination": "/w"}]}`,
			request: `{"Binds": ["/wd/sub/x:/x", "/wdx:/y"], "Mounts": [{"Type": "bind", "Source": "/wd/s", "Target": "/z"}]}`,
			want:    `{"Binds": ["/b/x:/x", "/m/wdx:/y"], "Mounts": [{"Type": "bind", "Source": "/a/s", "Target": "/z"}]}`,
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
			want:      "dond-shim: unsupported storage driver \"vfs\"",
		},
		{
			name:      "unsupported driver is fine when a mount matches",
			container: `{"Driver": "vfs", "Mounts": [{"Type": "bind", "Source": "/fixtures", "Destination": "/wd"}]}`,
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
			if strings.HasPrefix(tt.want, "dond-shim:") {
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

func TestRewriteCreateKeepsBodyWithNothingToMap(t *testing.T) {
	c := &container{Driver: "vfs"}
	for _, body := range []string{
		`{"Image":"busybox",  "HostConfig": {"Binds": ["v:/v"], "Unknown": true}}`,
		`{"Image": "busybox"}`,
		`{"HostConfig": null}`,
		`not json`,
	} {
		got, err := rewriteCreate([]byte(body), c.hostPath)
		if err != nil || string(got) != body {
			t.Errorf("rewriteCreate(%s) = %s, %v", body, got, err)
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
