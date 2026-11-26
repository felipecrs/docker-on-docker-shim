package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"path"
	"path/filepath"
	"slices"
	"strings"
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

// nestedMount is a mount of the container inside a requested source: source on the host, at rel
// below the requested destination.
type nestedMount struct {
	source, rel string
	readOnly    bool
}

// hostPath maps a path in the container to the host, tells whether the container sees it
// read-only, and lists the container's mounts inside it.
func (c *container) hostPath(source string) (string, bool, []nestedMount, error) {
	source = path.Clean(source)
	mapped, readOnly, longest := "", false, -1
	var nested []nestedMount
	inside := strings.TrimSuffix(source, "/") + "/"
	for _, m := range c.Mounts {
		if (m.Type != "bind" && m.Type != "volume") || m.Source == "" {
			continue
		}
		if (source == m.Destination || strings.HasPrefix(source, m.Destination+"/")) && len(m.Destination) > longest {
			mapped, readOnly, longest = m.Source+source[len(m.Destination):], !m.RW, len(m.Destination)
		}
		if strings.HasPrefix(m.Destination, inside) {
			nested = append(nested, nestedMount{m.Source, m.Destination[len(inside):], !m.RW})
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

// readOnlyOptions returns the options of a bind with ro in place of rw.
func readOnlyOptions(options string) string {
	list := slices.DeleteFunc(strings.Split(options, ","), func(o string) bool { return o == "" || o == "rw" })
	if !slices.Contains(list, "ro") {
		list = append(list, "ro")
	}
	return strings.Join(list, ",")
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

// rewriteCreate maps the bind sources in the body of POST /containers/create. Everything else is
// kept as it arrived, and a body with nothing to map is returned as is. A source the container
// sees read-only is bound read-only, as a nested Docker would.
func rewriteCreate(body []byte, mapPath func(string) (string, bool, []nestedMount, error)) ([]byte, error) {
	var doc, host map[string]json.RawMessage
	if json.Unmarshal(body, &doc) != nil || json.Unmarshal(doc["HostConfig"], &host) != nil {
		return body, nil
	}
	changed := false

	var binds []string
	if json.Unmarshal(host["Binds"], &binds) == nil && binds != nil {
		var out []string
		for _, bind := range binds {
			source, rest := splitBind(bind)
			destination, options, hasOptions := strings.Cut(rest, ":")
			if !isPath(source) || destination == "" {
				out = append(out, bind)
				continue
			}
			mapped, readOnly, nested, err := mapPath(source)
			if err != nil {
				return nil, err
			}
			rebuilt := mapped + ":" + rest
			if readOnly {
				rebuilt = mapped + ":" + destination + ":" + readOnlyOptions(options)
			}
			out = append(out, rebuilt)
			if rebuilt != bind {
				debugf("bind %s -> %s", bind, rebuilt)
				changed = true
			}
			for _, n := range nested {
				extra := n.source + ":" + destination + "/" + n.rel
				if n.readOnly {
					extra += ":" + readOnlyOptions(options)
				} else if hasOptions {
					extra += ":" + options
				}
				debugf("bind %s -> + %s", bind, extra)
				out = append(out, extra)
				changed = true
			}
		}
		host["Binds"], _ = json.Marshal(out)
	}

	var mounts []map[string]json.RawMessage
	if json.Unmarshal(host["Mounts"], &mounts) == nil && mounts != nil {
		var out []map[string]json.RawMessage
		for _, mount := range mounts {
			out = append(out, mount)
			var kind, source, target string
			_ = json.Unmarshal(mount["Type"], &kind)
			_ = json.Unmarshal(mount["Source"], &source)
			_ = json.Unmarshal(mount["Target"], &target)
			if kind != "bind" || !isPath(source) {
				continue
			}
			mapped, readOnly, nested, err := mapPath(source)
			if err != nil {
				return nil, err
			}
			if mapped != source {
				debugf("mount %s -> %s", source, mapped)
				mount["Source"], _ = json.Marshal(mapped)
				changed = true
			}
			var requested bool
			_ = json.Unmarshal(mount["ReadOnly"], &requested)
			for _, n := range nested {
				extra := maps.Clone(mount)
				extra["Source"], _ = json.Marshal(n.source)
				extra["Target"], _ = json.Marshal(target + "/" + n.rel)
				debugf("mount %s -> + %s:%s", source, n.source, target+"/"+n.rel)
				if n.readOnly && !requested {
					debugf("mount %s -> read-only", n.source)
					extra["ReadOnly"], _ = json.Marshal(true)
				}
				out = append(out, extra)
				changed = true
			}
			if readOnly && !requested {
				debugf("mount %s -> read-only", source)
				mount["ReadOnly"], _ = json.Marshal(true)
				changed = true
			}
		}
		host["Mounts"], _ = json.Marshal(out)
	}

	if !changed {
		return body, nil
	}
	doc["HostConfig"], _ = json.Marshal(host)
	return json.Marshal(doc)
}
