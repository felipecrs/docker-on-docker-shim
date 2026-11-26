//go:build linux

package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"

	dockerproxy "github.com/felipecrs/docker-on-docker-proxy/proxy"
)

// proxyHeader is set on every response of a dond proxy, so that a dond proxy in front of it does
// not map the paths a second time.
const proxyHeader = "Dond-Proxy"

// containerIDLine matches the resolv.conf that Docker mounts into the container.
var containerIDLine = regexp.MustCompile(`/([a-z0-9]{12,128})/resolv.conf `)

type proxy struct {
	network, address string // upstream
	socket           string // the proxy's own socket
	mountinfo        string
	handler          http.Handler
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
	transport := dockerproxy.NewTransport(func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, p.network, p.address)
	})
	p.client = &http.Client{Transport: transport}
	p.handler = dockerproxy.New("dond", transport, p.mapPath, debugf)
	return p
}

// logger keeps net/http's own messages, such as a client that went away, out of the output.
func logger() *log.Logger {
	if debug {
		return log.New(os.Stderr, "DEBUG(dond): ", 0)
	}
	return log.New(io.Discard, "", 0)
}

func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(proxyHeader, version)
	p.handler.ServeHTTP(w, r)
}

// mapPath maps a source path in this container to the host.
func (p *proxy) mapPath(source string) (string, bool, []dockerproxy.NestedMount, error) {
	if !strings.HasPrefix(source, "/") {
		return source, false, nil, nil
	}
	p.mu.Lock()
	if !p.inspected || (p.self == nil && !p.chained && p.retry) {
		p.inspected = true
		p.inspectSelf()
	}
	self, chained := p.self, p.chained
	p.mu.Unlock()
	if self == nil && !chained {
		return source, false, nil, nil
	}
	// Testcontainers mounts the socket that DOCKER_HOST names into its Ryuk container.
	if source == p.socket && p.network == "unix" {
		source = p.address
	}
	if chained {
		return source, false, nil, nil
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
