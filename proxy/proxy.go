// Package proxy forwards the Docker API to a daemon and maps the sources of the bind mounts of the
// containers it creates, through a function that the caller gives. dond, of this module, uses it
// for docker-on-docker, and docker-wsl-proxy for clients of Windows with Docker in WSL.
//
// The API exists for docker-wsl-proxy and may change in any v0 release.
package proxy

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
	"regexp"
	"strconv"
	"time"
)

// maxCreateBody is the largest create body read into memory. A larger one is forwarded unchanged.
const maxCreateBody = 4 << 20

var createPath = regexp.MustCompile(`^(/v[0-9.]+)?/containers/create$`)

// NewTransport returns a transport to the Docker daemon that dial connects to, for New and for the
// caller's own requests, which use any http URL, such as http://docker/version.
func NewTransport(dial func(ctx context.Context) (net.Conn, error)) *http.Transport {
	return &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dial(ctx)
		},
		DisableCompression: true,
		// An idle connection may hold a process on the other side, such as wsl.exe.
		IdleConnTimeout: 90 * time.Second,
	}
}

type handler struct {
	name    string
	mapPath MapFunc
	debugf  func(format string, args ...any)
	reverse *httputil.ReverseProxy
}

// New returns a handler that forwards every request through transport, and maps the bind sources
// of POST /containers/create through mapPath: those of HostConfig.Binds, and those of the bind
// entries of HostConfig.Mounts. The sources are paths of Linux or of Windows, never volume names.
// A create body over 4 MiB is forwarded unchanged. name starts the error messages that the handler
// returns to clients, an error of mapPath is returned to the client as is, and debugf prints a
// line of debug output.
func New(name string, transport http.RoundTripper, mapPath MapFunc, debugf func(format string, args ...any)) http.Handler {
	return &handler{name, mapPath, debugf, &httputil.ReverseProxy{
		Rewrite:       func(r *httputil.ProxyRequest) { r.Out.URL.Scheme, r.Out.URL.Host = "http", "docker" },
		FlushInterval: -1,
		Transport:     transport,
		// net/http's own messages, such as a client that went away, are debug output.
		ErrorLog: log.New(debugWriter(debugf), "", 0),
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			debugf("%s %s: %v", r.Method, r.URL.Path, err)
			writeError(w, http.StatusBadGateway, fmt.Errorf("%s: %w", name, err))
		},
	}}
}

type debugWriter func(format string, args ...any)

func (d debugWriter) Write(p []byte) (int, error) {
	d("%s", bytes.TrimSuffix(p, []byte("\n")))
	return len(p), nil
}

func writeError(w http.ResponseWriter, status int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"message": err.Error()})
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost && createPath.MatchString(r.URL.Path) {
		body, err := io.ReadAll(io.LimitReader(r.Body, maxCreateBody+1))
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("%s: %w", h.name, err))
			return
		}
		if len(body) > maxCreateBody {
			h.debugf("create body over %d bytes, not rewritten", maxCreateBody)
			r.Body = struct {
				io.Reader
				io.Closer
			}{io.MultiReader(bytes.NewReader(body), r.Body), r.Body}
		} else {
			rewritten, err := rewriteCreate(body, h.mapPath, h.debugf)
			if err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
			r.Body, r.ContentLength, r.TransferEncoding = io.NopCloser(bytes.NewReader(rewritten)), int64(len(rewritten)), nil
			r.Header.Set("Content-Length", strconv.Itoa(len(rewritten)))
		}
	}
	h.reverse.ServeHTTP(w, r)
}
