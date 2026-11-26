package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"path"
	"strings"
)

// container is what the shim needs from the inspection of the container it runs in.
type container struct {
	ID             string `json:"Id"`
	Image          string
	Driver         string
	ResolvConfPath string
	GraphDriver    struct{ Data map[string]string }
	Mounts         []struct{ Type, Source, Destination string }
}

// nestedMount is a mount of the container inside a requested source: source on the host, at rel
// below the requested destination.
type nestedMount struct{ source, rel string }

// hostPath maps a path in the container to the host, and lists the container's mounts inside it.
func (c *container) hostPath(source string) (string, []nestedMount, error) {
	source = path.Clean(source)
	mapped, longest := "", -1
	var nested []nestedMount
	inside := strings.TrimSuffix(source, "/") + "/"
	for _, m := range c.Mounts {
		if (m.Type != "bind" && m.Type != "volume") || m.Source == "" {
			continue
		}
		if (source == m.Destination || strings.HasPrefix(source, m.Destination+"/")) && len(m.Destination) > longest {
			mapped, longest = m.Source+source[len(m.Destination):], len(m.Destination)
		}
		if strings.HasPrefix(m.Destination, inside) {
			nested = append(nested, nestedMount{m.Source, m.Destination[len(inside):]})
		}
	}
	if longest < 0 {
		root, err := c.root()
		if err != nil {
			return "", nil, err
		}
		mapped = root + source
	}
	return mapped, nested, nil
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
// kept as it arrived, and a body with nothing to map is returned as is.
func rewriteCreate(body []byte, mapPath func(string) (string, []nestedMount, error)) ([]byte, error) {
	var doc, host map[string]json.RawMessage
	if json.Unmarshal(body, &doc) != nil || json.Unmarshal(doc["HostConfig"], &host) != nil {
		return body, nil
	}
	changed := false

	var binds []string
	if json.Unmarshal(host["Binds"], &binds) == nil && binds != nil {
		var out []string
		for _, bind := range binds {
			source, rest, _ := strings.Cut(bind, ":")
			destination, options, hasOptions := strings.Cut(rest, ":")
			if !strings.HasPrefix(source, "/") || destination == "" {
				out = append(out, bind)
				continue
			}
			mapped, nested, err := mapPath(source)
			if err != nil {
				return nil, err
			}
			out = append(out, mapped+":"+rest)
			if mapped != source {
				debugf("bind %s -> %s", bind, out[len(out)-1])
				changed = true
			}
			for _, n := range nested {
				extra := n.source + ":" + destination + "/" + n.rel
				if hasOptions {
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
			if kind != "bind" || !strings.HasPrefix(source, "/") {
				continue
			}
			mapped, nested, err := mapPath(source)
			if err != nil {
				return nil, err
			}
			if mapped != source {
				debugf("mount %s -> %s", source, mapped)
				mount["Source"], _ = json.Marshal(mapped)
				changed = true
			}
			for _, n := range nested {
				extra := maps.Clone(mount)
				extra["Source"], _ = json.Marshal(n.source)
				extra["Target"], _ = json.Marshal(target + "/" + n.rel)
				debugf("mount %s -> + %s:%s", source, n.source, target+"/"+n.rel)
				out = append(out, extra)
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
