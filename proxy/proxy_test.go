package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
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

// startProxy starts a fake daemon and a proxy in front of it, over TCP so that it runs on Windows
// too, and returns the address of the proxy. down closes the daemon before the proxy starts.
func startProxy(t *testing.T, down bool) (proxyAddress string, received chan upstreamRequest) {
	received = make(chan upstreamRequest, 10)
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	t.Cleanup(daemon.Close)
	address := daemon.Listener.Addr().String()
	if down {
		daemon.Close()
	}
	transport := NewTransport(func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", address)
	})
	proxy := httptest.NewServer(New("test", transport, testMap, debugf))
	t.Cleanup(proxy.Close)
	return proxy.Listener.Addr().String(), received
}

func TestProxyPassesRequestsThrough(t *testing.T) {
	address, received := startProxy(t, false)
	for _, tt := range []struct{ method, uri, body string }{
		{"GET", "/v1.51/containers/json?all=1", ""},
		{"POST", "/v1.51/containers/abc/start", `{"HostConfig": {"Binds": ["/a:/a"]}}`},
		// A create with nothing to map, sent byte for byte.
		{"POST", "/containers/create?name=x", `{"Image":"busybox",  "HostConfig": {"Binds": ["v:/v"]}}`},
		// A create over the limit, forwarded unchanged.
		{"POST", "/containers/create", `{"HostConfig": {"Binds": ["/a:/a"]}, "Labels": {"x": "` + strings.Repeat("x", maxCreateBody) + `"}}`},
	} {
		req, _ := http.NewRequest(tt.method, "http://"+address+tt.uri, strings.NewReader(tt.body))
		req.Header.Set("X-Test", "kept")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		answer, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		got := <-received
		if got.method != tt.method || got.uri != tt.uri || string(got.body) != tt.body || got.header != "kept" {
			t.Errorf("upstream got %s %s %.80q %q, want %s %s %.80q", got.method, got.uri, got.body, got.header, tt.method, tt.uri, tt.body)
		}
		if resp.StatusCode != http.StatusCreated || resp.Header.Get("X-Answer") != "yes" || string(answer) != "answer" {
			t.Errorf("client got %s %q %q", resp.Status, resp.Header, answer)
		}
	}
}

func TestProxyRewritesCreate(t *testing.T) {
	address, received := startProxy(t, false)
	body := `{"Image": "busybox", "HostConfig": {"Binds": ["/a:/a"]}}`
	// Without a length, the client sends the body in chunks.
	req, _ := http.NewRequest("POST", "http://"+address+"/v1.51/containers/create?name=x", io.NopCloser(strings.NewReader(body)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	got := <-received
	want := `{"Image": "busybox", "HostConfig": {"Binds": ["/host/a:/a"]}}`
	if got.uri != "/v1.51/containers/create?name=x" || !jsonEqual(t, got.body, []byte(want)) {
		t.Errorf("upstream got %s %s, want %s", got.uri, got.body, want)
	}
	if got.contentLength != strconv.Itoa(len(got.body)) {
		t.Errorf("Content-Length %q for %d bytes", got.contentLength, len(got.body))
	}
}

func TestProxyErrors(t *testing.T) {
	for _, tt := range []struct {
		name, body string
		down       bool
		status     int
		message    string
	}{
		{"mapping", `{"HostConfig": {"Binds": ["/fail:/x"]}}`, false, http.StatusInternalServerError, "test: cannot map /fail"},
		{"daemon down", `{}`, true, http.StatusBadGateway, "test: dial tcp "},
	} {
		t.Run(tt.name, func(t *testing.T) {
			address, received := startProxy(t, tt.down)
			resp, err := http.Post("http://"+address+"/v1.24/containers/create", "application/json", strings.NewReader(tt.body))
			if err != nil {
				t.Fatal(err)
			}
			var answer struct{ Message string }
			_ = json.NewDecoder(resp.Body).Decode(&answer)
			resp.Body.Close()
			if resp.StatusCode != tt.status || !strings.HasPrefix(answer.Message, tt.message) {
				t.Errorf("got %s %q, want %d %q", resp.Status, answer.Message, tt.status, tt.message)
			}
			if len(received) != 0 {
				t.Error("the request reached the daemon")
			}
		})
	}
}

func TestProxyRelaysUpgradedConnections(t *testing.T) {
	address, _ := startProxy(t, false)
	conn, err := net.Dial("tcp", address)
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
	if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	if string(data) != "got ping" {
		t.Errorf("got %q, %v, want %q", data, err, "got ping")
	}
}
