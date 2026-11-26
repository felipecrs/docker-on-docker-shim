//go:build linux

package main

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"

	dockerproxy "github.com/felipecrs/docker-on-docker-shim/proxy"
)

// container is what the shim needs from the inspection of the container it runs in.
type container struct {
	ID             string `json:"Id"`
	Image          string
	Driver         string
	ResolvConfPath string
	GraphDriver    struct{ Data map[string]string }
	Mounts         []struct {
		Type, Source, Destination string
		RW                        bool
	}
	HostConfig struct {
		UsernsMode     string
		ReadonlyRootfs bool
	}
}

// hostPath maps a path in the container to the host, tells whether the container sees it
// read-only, and lists the container's mounts inside it.
func (c *container) hostPath(source string) (string, bool, []dockerproxy.NestedMount, error) {
	source = path.Clean(source)
	mapped, readOnly, longest := "", false, -1
	var nested []dockerproxy.NestedMount
	inside := strings.TrimSuffix(source, "/") + "/"
	for _, m := range c.Mounts {
		if (m.Type != "bind" && m.Type != "volume") || m.Source == "" {
			continue
		}
		if (source == m.Destination || strings.HasPrefix(source, m.Destination+"/")) && len(m.Destination) > longest {
			mapped, readOnly, longest = m.Source+source[len(m.Destination):], !m.RW, len(m.Destination)
		}
		if strings.HasPrefix(m.Destination, inside) {
			nested = append(nested, dockerproxy.NestedMount{Source: m.Source, Rel: m.Destination[len(inside):], ReadOnly: !m.RW})
		}
	}
	if longest < 0 {
		root, err := c.root()
		if err != nil {
			return "", false, nil, err
		}
		mapped, readOnly = root+source, c.HostConfig.ReadonlyRootfs
	}
	return mapped, readOnly, nested, nil
}

// mountHolding returns the destination of the bind or volume mount that holds path, or "".
func (c *container) mountHolding(path string) string {
	for _, m := range c.Mounts {
		// The inspection has the destination as given, such as /var/run/docker.sock for /run/docker.sock.
		destination, err := filepath.EvalSymlinks(m.Destination)
		if err != nil {
			destination = m.Destination
		}
		if (m.Type == "bind" || m.Type == "volume") && (path == destination || strings.HasPrefix(path, destination+"/")) {
			return m.Destination
		}
	}
	return ""
}

// root returns the container's root directory on the host.
func (c *container) root() (string, error) {
	switch c.Driver {
	case "overlay2":
		if dir := c.GraphDriver.Data["MergedDir"]; dir != "" {
			return dir, nil
		}
	case "overlayfs": // containerd image store
		if i := strings.LastIndex(c.ResolvConfPath, "/containers/"); i > 0 {
			return c.ResolvConfPath[:i] + "/rootfs/overlayfs/" + c.ID, nil
		}
	}
	return "", fmt.Errorf("dond-shim: unsupported storage driver %q, cannot find the container's root on the host", c.Driver)
}
