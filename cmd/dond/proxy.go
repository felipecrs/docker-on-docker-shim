package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"regexp"
	"strconv"
	"sync"
)

const (
	// maxCreateBody is the largest create body read into memory. A larger one is forwarded unchanged.
	maxCreateBody = 4 << 20
	// proxyHeader is set on every response of a dond proxy, so that a dond proxy in front of it
	// does not map the paths a second time.
	proxyHeader = "Dond-Shim-Proxy"
)

var (
	createPath = regexp.MustCompile(`^(/v[0-9.]+)?/containers/create$`)
	// containerIDLine matches the resolv.conf that Docker mounts into the container.
	containerIDLine = regexp.MustCompile(`/([a-z0-9]{12,128})/resolv.conf `)
)

type proxy struct {
	network, address string // upstream
	socket           string // the proxy's own socket
	mountinfo        string
	reverse          *httputil.ReverseProxy
	client           *http.Client

	retry bool // inspect again after a failed inspection, for a proxy that lives long

	mu        sync.Mutex
	inspected bool
	self      *container // nil when the daemon does not know the container this runs in
	chained   bool       // the upstream is a dond proxy, which maps the paths itself
}

func newProxy(upstream, socket string) *proxy {
	p := &proxy{socket: socket, mountinfo: "/proc/self/mountinfo"}
	p.network, p.address = dialAddress(upstream)
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, p.network, p.address)
		},
		DisableCompression: true,
	}
	p.client = &http.Client{Transport: transport}
	p.reverse = &httputil.ReverseProxy{
		Rewrite:       func(r *httputil.ProxyRequest) { r.Out.URL.Scheme, r.Out.URL.Host = "http", "docker" },
		FlushInterval: -1,
		Transport:     transport,
		ErrorLog:      logger(),
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			debugf("%s %s: %v", r.Method, r.URL.Path, err)
			writeError(w, http.StatusBadGateway, fmt.Errorf("dond-shim: %w", err))
		},
	}
	return p
}

// logger keeps net/http's own messages, such as a client that went away, out of the output.
func logger() *log.Logger {
	if debug {
		return log.New(os.Stderr, "DEBUG(dond-shim): ", 0)
	}
	return log.New(io.Discard, "", 0)
}

func writeError(w http.ResponseWriter, status int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"message": err.Error()})
}

func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(proxyHeader, version)
	if r.Method == http.MethodPost && createPath.MatchString(r.URL.Path) {
		body, err := io.ReadAll(io.LimitReader(r.Body, maxCreateBody+1))
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("dond-shim: %w", err))
			return
		}
		if len(body) > maxCreateBody {
			debugf("create body over %d bytes, not rewritten", maxCreateBody)
			r.Body = struct {
				io.Reader
				io.Closer
			}{io.MultiReader(bytes.NewReader(body), r.Body), r.Body}
		} else {
			rewritten, err := rewriteCreate(body, p.mapPath)
			if err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
			r.Body, r.ContentLength, r.TransferEncoding = io.NopCloser(bytes.NewReader(rewritten)), int64(len(rewritten)), nil
			r.Header.Set("Content-Length", strconv.Itoa(len(rewritten)))
		}
	}
	p.reverse.ServeHTTP(w, r)
}

// mapPath maps a source path in this container to the host.
func (p *proxy) mapPath(source string) (string, []nestedMount, error) {
	p.mu.Lock()
	if !p.inspected || (p.self == nil && !p.chained && p.retry) {
		p.inspected = true
		p.inspectSelf()
	}
	self, chained := p.self, p.chained
	p.mu.Unlock()
	if self == nil && !chained {
		return source, nil, nil
	}
	// Testcontainers mounts the socket that DOCKER_HOST names into its Ryuk container.
	if source == p.socket && p.network == "unix" {
		source = p.address
	}
	if chained {
		return source, nil, nil
	}
	return self.hostPath(source)
}

func (p *proxy) inspectSelf() {
	data, err := os.ReadFile(p.mountinfo)
	var id string
	for _, match := range containerIDLine.FindAllSubmatch(data, -1) {
		id = string(match[1])
	}
	if id == "" {
		debugf("not in a container (%v), not rewriting", err)
		return
	}
	resp, err := p.client.Get("http://docker/containers/" + id + "/json")
	if err != nil {
		debugf("inspect %s: %v, not rewriting", id, err)
		return
	}
	defer resp.Body.Close()
	var c container
	if resp.Header.Get(proxyHeader) != "" {
		debugf("the upstream is a dond proxy, which maps the paths itself")
		p.chained = true
	} else if resp.StatusCode != http.StatusOK {
		debugf("inspect %s: %s, not rewriting", id, resp.Status)
	} else if err := json.NewDecoder(resp.Body).Decode(&c); err != nil {
		debugf("inspect %s: %v, not rewriting", id, err)
	} else {
		p.self = &c
	}
}
