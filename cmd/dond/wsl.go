package main

import (
	"regexp"
	"strings"
)

// The rules for the paths of Windows come from docker-wsl-shim.
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

// wslPath maps a path of Windows to WSL: C:\dir to <root>c/dir, and \\wsl.localhost\<distro>\dir to
// /dir. Anything else, a path of WSL included, is returned as is. root is called only for a drive.
//
// shortcut: \\wsl.localhost\<distro> is taken for the distribution of the daemon.
func wslPath(p string, root func() string) string {
	switch {
	case drivePath.MatchString(p):
		p = strings.TrimSuffix(root(), "/") + "/" + strings.ToLower(p[:1]) + p[2:]
	case wslShare.MatchString(p):
		if p = wslShare.ReplaceAllString(p, ""); p == "" {
			p = "/"
		}
	default:
		return p
	}
	return strings.ReplaceAll(p, `\`, "/")
}

// automountRoot returns the root of the drives that the wsl.conf of a distribution sets, or /mnt/.
func automountRoot(conf string) string {
	root, section := "/mnt/", ""
	for line := range strings.Lines(conf) {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			section = strings.ToLower(strings.Trim(line, "[] "))
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || section != "automount" || !strings.EqualFold(strings.TrimSpace(key), "root") {
			continue
		}
		if i := strings.IndexAny(value, "#;"); i >= 0 {
			value = value[:i]
		}
		if value = strings.Trim(strings.TrimSpace(value), `"`); strings.HasPrefix(value, "/") {
			root = value
		}
	}
	return root
}
