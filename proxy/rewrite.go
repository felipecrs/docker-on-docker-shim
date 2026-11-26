package proxy

import (
	"encoding/json"
	"maps"
	"regexp"
	"slices"
	"strings"
)

// MapFunc maps the source of a bind to the host of the daemon. readOnly makes the bind read-only,
// whatever the request asked, and nested are more binds to add below its destination.
type MapFunc func(source string) (mapped string, readOnly bool, nested []NestedMount, err error)

// NestedMount is a bind that a MapFunc adds below the destination of the bind it maps: Source on
// the host, at Rel below the destination.
type NestedMount struct {
	Source, Rel string
	ReadOnly    bool
}

// The paths of Windows are sources too, for docker-wsl-proxy. The rules come from docker-wsl-shim.
var (
	drivePath = regexp.MustCompile(`^[A-Za-z]:[\\/]`)
	wslShare  = regexp.MustCompile(`(?i)^[\\/]{2}(?:wsl\$|wsl\.localhost)[\\/][^\\/]+`)
)

func isWindowsPath(s string) bool {
	return drivePath.MatchString(s) || wslShare.MatchString(s)
}

// isPath tells whether a bind source is a path, of Linux or of Windows, and not a volume name.
func isPath(s string) bool {
	return strings.HasPrefix(s, "/") || isWindowsPath(s)
}

// splitBind splits a bind such as C:\src:/dst:ro at the colon after the source.
func splitBind(bind string) (source, rest string) {
	from := 0
	if drivePath.MatchString(bind) {
		from = 2
	}
	if i := strings.IndexByte(bind[from:], ':'); i >= 0 {
		return bind[:from+i], bind[from+i+1:]
	}
	return bind, ""
}

// readOnlyOptions returns the options of a bind with ro in place of rw.
func readOnlyOptions(options string) string {
	list := slices.DeleteFunc(strings.Split(options, ","), func(o string) bool { return o == "" || o == "rw" })
	if !slices.Contains(list, "ro") {
		list = append(list, "ro")
	}
	return strings.Join(list, ",")
}

// rewriteCreate maps the bind sources in the body of POST /containers/create. Everything else is
// kept as it arrived, and a body with nothing to map is returned as is. A source that mapPath
// says is read-only is bound read-only.
func rewriteCreate(body []byte, mapPath MapFunc, debugf func(format string, args ...any)) ([]byte, error) {
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
				extra := n.Source + ":" + destination + "/" + n.Rel
				if n.ReadOnly {
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
				extra["Source"], _ = json.Marshal(n.Source)
				extra["Target"], _ = json.Marshal(target + "/" + n.Rel)
				debugf("mount %s -> + %s:%s", source, n.Source, target+"/"+n.Rel)
				if n.ReadOnly && !requested {
					debugf("mount %s -> read-only", n.Source)
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
