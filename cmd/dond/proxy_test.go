package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// upstreamRequest is what the fake daemon received.
type upstreamRequest struct {
	method, uri, contentLength, header string
	body                               []byte
}

// startProxy starts a fake daemon and a proxy in front of it. The daemon knows the container
// knownid0123456789, inspected as inspect with UPSTREAM replaced by the daemon's socket, and the
// proxy runs in the container that mountinfoID names.
func startProxy(t *testing.T, mountinfoID, inspect string) (client *http.Client, socket, upstreamSocket string, received chan upstreamRequest) {
	dir := t.TempDir()
	upstreamSocket, socket = filepath.Join(dir, "up.sock"), filepath.Join(dir, "proxy.sock")
	received = make(chan upstreamRequest, 10)
	serve(t, upstreamSocket, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/containers/") && strings.HasSuffix(r.URL.Path, "id0123456789/json") {
			if r.URL.Path != "/containers/knownid0123456789/json" || inspect == "" {
				http.Error(w, `{"message": "No such container"}`, http.StatusNotFound)
				return
			}
			_, _ = io.WriteString(w, strings.ReplaceAll(inspect, "UPSTREAM", upstreamSocket))
			return
		}
		if r.Header.Get("Upgrade") == "tcp" {
			conn, buf, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			defer conn.Close()
			_, _ = io.WriteString(conn, "HTTP/1.1 101 UPGRADED\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
			// Answers only once the client closed its side.
			data, _ := io.ReadAll(buf)
			_, _ = io.WriteString(conn, "got "+string(data))
			return
		}
		body, _ := io.ReadAll(r.Body)
		received <- upstreamRequest{r.Method, r.RequestURI, r.Header.Get("Content-Length"), r.Header.Get("X-Test"), body}
		w.Header().Set("X-Answer", "yes")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, "answer")
	}))

	mountinfo := filepath.Join(dir, "mountinfo")
	writeFile(t, mountinfo, "1 2 0:1 / / rw - overlay overlay rw\n"+
		"3 1 8:1 /var/lib/docker/containers/"+mountinfoID+"/resolv.conf /etc/resolv.conf rw - ext4 /dev/sda1 rw\n")
	p := newProxy("unix://"+upstreamSocket, socket)
	p.mountinfo = mountinfo
	serve(t, socket, p)
	return unixClient(socket), socket, upstreamSocket, received
}

func serve(t *testing.T, socket string, handler http.Handler) {
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })
}

func unixClient(socket string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}}
}

func TestProxyPassesRequestsThrough(t *testing.T) {
	client, _, _, received := startProxy(t, "knownid0123456789", `{"Driver": "overlay2", "GraphDriver": {"Data": {"MergedDir": "/merged"}}}`)
	for _, tt := range []struct{ method, uri, body string }{
		{"GET", "/v1.51/containers/json?all=1", ""},
		{"POST", "/v1.51/containers/abc/start", `{"HostConfig": {"Binds": ["/a:/a"]}}`},
		// A create with nothing to map, sent byte for byte.
		{"POST", "/containers/create?name=x", `{"Image":"busybox",  "HostConfig": {"Binds": ["v:/v"]}}`},
	} {
		req, _ := http.NewRequest(tt.method, "http://docker"+tt.uri, strings.NewReader(tt.body))
		req.Header.Set("X-Test", "kept")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		answer, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		got := <-received
		if got.method != tt.method || got.uri != tt.uri || string(got.body) != tt.body || got.header != "kept" {
			t.Errorf("upstream got %s %s %q %q, want %s %s %q", got.method, got.uri, got.body, got.header, tt.method, tt.uri, tt.body)
		}
		if resp.StatusCode != http.StatusCreated || resp.Header.Get("X-Answer") != "yes" || string(answer) != "answer" {
			t.Errorf("client got %s %q %q", resp.Status, resp.Header, answer)
		}
	}
}

func TestProxyRewritesCreate(t *testing.T) {
	const inspect = `{"Id": "knownid0123456789", "Driver": "overlay2", "GraphDriver": {"Data": {"MergedDir": "/merged"}}, "Mounts": [
		{"Type": "bind", "Source": "/fixtures", "Destination": "/wd"},
		{"Type": "bind", "Source": "/host/docker.sock", "Destination": "UPSTREAM"}]}`
	client, socket, _, received := startProxy(t, "knownid0123456789", inspect)
	body := `{"Image": "busybox", "HostConfig": {"Binds": ["/wd/x:/x", "` + socket + `:/var/run/docker.sock"], "Mounts": [{"Type": "bind", "Source": "/only-here", "Target": "/o"}]}}`
	// Without a length, the client sends the body in chunks.
	req, _ := http.NewRequest("POST", "http://docker/v1.51/containers/create?name=x", io.NopCloser(strings.NewReader(body)))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	got := <-received
	want := `{"Image": "busybox", "HostConfig": {"Binds": ["/fixtures/x:/x", "/host/docker.sock:/var/run/docker.sock"], "Mounts": [{"Type": "bind", "Source": "/merged/only-here", "Target": "/o"}]}}`
	if got.uri != "/v1.51/containers/create?name=x" || !jsonEqual(t, got.body, []byte(want)) {
		t.Errorf("upstream got %s %s, want %s", got.uri, got.body, want)
	}
	if got.contentLength != strconv.Itoa(len(got.body)) {
		t.Errorf("Content-Length %q for %d bytes", got.contentLength, len(got.body))
	}
}

func TestProxyBehindAnotherProxyMapsOnce(t *testing.T) {
	_, daemonSocket, upstreamSocket, received := startProxy(t, "knownid0123456789", `{"Driver": "overlay2", "GraphDriver": {"Data": {"MergedDir": "/merged"}}}`)
	// A wrapper in the same container, in front of the proxy of the daemon.
	socket := filepath.Join(t.TempDir(), "wrapper.sock")
	p := newProxy("unix://"+daemonSocket, socket)
	p.mountinfo = filepath.Join(filepath.Dir(daemonSocket), "mountinfo")
	serve(t, socket, p)
	body := `{"HostConfig": {"Binds": ["/only-here:/o", "` + socket + `:/var/run/docker.sock"]}}`
	resp, err := unixClient(socket).Post("http://docker/containers/create", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Header.Get(proxyHeader) == "" {
		t.Errorf("no %s header", proxyHeader)
	}
	got := <-received
	// The wrapper names the socket of the daemon's proxy, which names its own upstream.
	want := `{"HostConfig": {"Binds": ["/merged/only-here:/o", "/merged` + upstreamSocket + `:/var/run/docker.sock"]}}`
	if !jsonEqual(t, got.body, []byte(want)) {
		t.Errorf("upstream got %s, want %s", got.body, want)
	}
}

func TestProxyLeavesCreateAloneWhenTheDaemonDoesNotKnowTheContainer(t *testing.T) {
	client, _, _, received := startProxy(t, "unknownid0123456789", "")
	body := `{"Image": "busybox", "HostConfig": {"Binds": ["/wd:/wd"]}}`
	resp, err := client.Post("http://docker/containers/create", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := <-received; string(got.body) != body {
		t.Errorf("upstream got %s, want %s", got.body, body)
	}
}

func TestProxyRefusesCreateWithUnsupportedDriver(t *testing.T) {
	client, _, _, received := startProxy(t, "knownid0123456789", `{"Driver": "vfs"}`)
	resp, err := client.Post("http://docker/v1.24/containers/create", "application/json", strings.NewReader(`{"HostConfig": {"Binds": ["/x:/x"]}}`))
	if err != nil {
		t.Fatal(err)
	}
	var answer struct{ Message string }
	_ = json.NewDecoder(resp.Body).Decode(&answer)
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError || !strings.HasPrefix(answer.Message, `dond-shim: unsupported storage driver "vfs"`) {
		t.Errorf("got %s %q", resp.Status, answer.Message)
	}
	if len(received) != 0 {
		t.Error("the request reached the daemon")
	}
}

func TestProxyRelaysUpgradedConnections(t *testing.T) {
	_, socket, _, _ := startProxy(t, "knownid0123456789", "{}")
	conn, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	// Docker sends hijack requests with an empty Host header.
	_, _ = io.WriteString(conn, "POST /v1.51/containers/abc/attach?stream=1&stdin=1 HTTP/1.1\r\nHost: \r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status %s", resp.Status)
	}
	_, _ = io.WriteString(conn, "ping")
	if err := conn.(*net.UnixConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	if string(data) != "got ping" {
		t.Errorf("got %q, %v, want %q", data, err, "got ping")
	}
}
