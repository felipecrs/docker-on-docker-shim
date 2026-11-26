//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const dockerSock = "/var/run/docker.sock"

// daemon serves the proxy at /var/run/docker.sock until it is stopped, and then leaves a symlink to
// the real socket there, so that docker keeps working.
func daemon() (int, error) {
	dir, err := filepath.EvalSymlinks(filepath.Dir(dockerSock))
	if err != nil {
		return 1, err
	}
	// docker-host.sock is where the docker-outside-of-docker feature of devcontainers mounts the real
	// socket, and then the proxy can take the place of docker.sock at once.
	path, hostSock := filepath.Join(dir, "docker.sock"), filepath.Join(dir, "docker-host.sock")
	mountinfo, _ := os.ReadFile("/proc/self/mountinfo")
	route, err := startState(path, hostSock, mountinfo)
	if err != nil {
		return 1, err
	}
	upstream := hostSock
	if route == "mount" {
		upstream = path // until the rename
	}
	p := newProxy("unix://"+upstream, "")
	// Every route writes in dir, which must not be a directory of the host.
	if err := p.ownDir(dir, mountinfo); err != nil {
		return 1, err
	}
	// The socket is made aside first: renaming the mount is only worth it once the proxy can listen,
	// and renaming the socket into place leaves no moment without a socket at the path.
	tmp := path + ".dond"
	_ = os.Remove(tmp)
	listener, err := net.Listen("unix", tmp)
	if err != nil {
		return 1, err
	}
	listener.(*net.UnixListener).SetUnlinkOnClose(false)
	listen := path
	if route == "mount" {
		start := time.Now()
		if route, err = p.takeOver(dir, hostSock); err == nil {
			p.address = hostSock
			route += fmt.Sprintf(", %d ms", time.Since(start).Milliseconds())
		} else {
			listen = filepath.Join(dir, "dond.sock")
			p, route = newProxy("unix://"+upstream, listen), "side socket"
			fmt.Fprintf(os.Stderr, "WARNING(dond-shim): cannot take the place of %s: %v\n", dockerSock, err)
			fmt.Fprintf(os.Stderr, "WARNING(dond-shim): serving at %s instead: set DOCKER_HOST=unix://%s for the clients\n", listen, listen)
		}
	}
	p.retry = true

	info, err := os.Stat(p.address)
	if err == nil {
		stat := info.Sys().(*syscall.Stat_t)
		if err = os.Chown(tmp, int(stat.Uid), int(stat.Gid)); err == nil {
			// With the special bits: rootless Docker sets the sticky bit on its socket.
			if err = os.Chmod(tmp, info.Mode()&(fs.ModePerm|fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky)); err == nil {
				err = os.Rename(tmp, listen)
			}
		}
	}
	if err != nil {
		_ = os.Remove(tmp)
		if listen == path {
			_ = os.Symlink(filepath.Base(hostSock), path)
		}
		return 1, err
	}
	fmt.Fprintf(os.Stderr, "dond-shim %s: serving at %s, forwarding to %s (%s)\n", version, listen, p.address, route)

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	served := make(chan error, 1)
	go func() { served <- (&http.Server{Handler: p, ErrorLog: logger()}).Serve(listener) }()
	select {
	case <-signals:
	case err = <-served:
	}
	if listen == path {
		// Put the symlink back, so that docker keeps working without the proxy.
		if err2 := os.Symlink(filepath.Base(hostSock), tmp); err2 != nil {
			err = errors.Join(err, err2)
		} else {
			err = errors.Join(err, os.Rename(tmp, path))
		}
	} else {
		err = errors.Join(err, os.Remove(listen))
	}
	if err != nil {
		return 1, err
	}
	return 0, nil
}

// startState tells what is at path, the place of /var/run/docker.sock: "mount" for the mount of the
// real socket, which must be moved to hostSock first, or else "symlink" (to hostSock, in any form),
// "dead socket" (left by a daemon that was killed) or "nothing", where the proxy can take the place
// at once.
func startState(path, hostSock string, mountinfo []byte) (string, error) {
	if mountPoint(path, mountinfo) == path {
		return "mount", nil
	}
	if !alive(hostSock) {
		return "", fmt.Errorf("no Docker socket at %s or %s: start the container with -v %s:%s", path, hostSock, dockerSock, dockerSock)
	}
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "nothing", nil
	case err != nil:
		return "", err
	case info.Mode()&fs.ModeSymlink != 0:
		target, err1 := os.Stat(path)
		host, err2 := os.Stat(hostSock)
		if err1 == nil && err2 == nil && os.SameFile(target, host) {
			return "symlink", nil
		}
	case info.Mode()&fs.ModeSocket != 0:
		if conn, err := net.Dial("unix", path); err == nil {
			conn.Close()
		} else if errors.Is(err, syscall.ECONNREFUSED) {
			return "dead socket", nil
		}
	}
	return "", fmt.Errorf("%s is in use by something else, such as another dond daemon", path)
}

// mountPoint returns the mount point in mountinfo that holds path.
func mountPoint(path string, mountinfo []byte) string {
	point := ""
	for _, line := range strings.Split(string(mountinfo), "\n") {
		if fields := strings.Fields(line); len(fields) > 4 && len(fields[4]) > len(point) &&
			(fields[4] == "/" || path == fields[4] || strings.HasPrefix(path, fields[4]+"/")) {
			point = fields[4]
		}
	}
	return point
}

// ownDir returns an error when dir may be a directory of the host, a bind or volume mount of the
// container, where a rename or a new file would change the host. The API is needed only when dir is
// not on the container's root filesystem.
func (p *proxy) ownDir(dir string, mountinfo []byte) error {
	if mountPoint(dir, mountinfo) == "/" {
		return nil
	}
	p.inspectSelf()
	p.inspected = true
	if p.self == nil {
		return fmt.Errorf("%s is a mount, and the container this runs in cannot be inspected to tell whether it belongs to the host (DOND_SHIM_DEBUG=true tells why)", dir)
	}
	if destination := p.self.mountHolding(dir); destination != "" {
		return fmt.Errorf("%s is in the mount at %s, which may be a directory of the host: refusing to write there", dir, destination)
	}
	return nil
}

func alive(socket string) bool {
	conn, err := net.Dial("unix", socket)
	if err == nil {
		conn.Close()
	}
	return err == nil
}

// takeOver moves the mount of the real socket at dir/docker.sock to hostSock. The mount namespace
// that has a mount cannot rename it, so a short helper container does it from its own. It returns
// the route that worked.
func (p *proxy) takeOver(dir, hostSock string) (string, error) {
	if os.Geteuid() != 0 {
		return "", errors.New("dond daemon must run as root")
	}
	if !p.inspected {
		p.inspectSelf()
		p.inspected = true
	}
	if p.self == nil {
		return "", errors.New("cannot inspect the container this runs in (DOND_SHIM_DEBUG=true tells why)")
	}
	// The root of this process, seen from a container in the same PID namespace. /proc/1/root is
	// refused when the main process of the container is not root.
	root := "/proc/" + strconv.Itoa(os.Getpid()) + "/root"
	errPID := p.helper(map[string]any{"PidMode": "container:" + p.self.ID}, root+dir+"/docker.sock", root+hostSock)
	if errPID == nil {
		return "helper through " + root, nil
	}
	debugf("helper through %s: %v", root, errPID)
	// Not possible when dir is a tmpfs, which the host does not see.
	hostRoot, err := p.self.root()
	if err != nil {
		return "", fmt.Errorf("through %s: %w; through the host: %w", root, errPID, err)
	}
	hostDir := hostRoot + dir
	errHost := p.helper(map[string]any{"Binds": []string{hostDir + ":/dond-run"}}, "/dond-run/docker.sock", "/dond-run/"+filepath.Base(hostSock))
	if errHost == nil {
		return "helper through " + hostDir + " on the host", nil
	}
	return "", fmt.Errorf("through %s: %w; through %s on the host: %w", root, errPID, hostDir, errHost)
}

// helper renames from to to in a short container from the image of this container, with this binary
// as entrypoint, or with mv when the image does not have it at the same path.
func (p *proxy) helper(host map[string]any, from, to string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	code, err := p.runHelper(host, []string{exe}, []string{renameCommand, from, to})
	if err == nil && code != 0 {
		return fmt.Errorf("rename: %w", syscall.Errno(code))
	}
	if err == nil {
		return nil
	}
	debugf("helper with %s: %v, trying mv", exe, err)
	if code, err = p.runHelper(host, []string{"mv"}, []string{from, to}); err == nil && code != 0 {
		return fmt.Errorf("mv exited with %d", code)
	}
	return err
}

// runHelper runs a container like docker run --rm --network none, and returns its exit code.
func (p *proxy) runHelper(host map[string]any, entrypoint, cmd []string) (int, error) {
	client := &http.Client{Transport: p.client.Transport, Timeout: time.Minute}
	call := func(method, path string, body, out any) error {
		var reader io.Reader = http.NoBody
		if body != nil {
			data, _ := json.Marshal(body)
			reader = bytes.NewReader(data)
		}
		req, _ := http.NewRequest(method, "http://docker"+path, reader)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 300 {
			var answer struct{ Message string }
			_ = json.NewDecoder(resp.Body).Decode(&answer)
			return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, answer.Message)
		}
		if out == nil {
			return nil
		}
		return json.NewDecoder(resp.Body).Decode(out)
	}

	host["AutoRemove"], host["NetworkMode"] = true, "none"
	// The same user namespace, so that root in the helper is the user that owns this container.
	host["UsernsMode"] = p.self.HostConfig.UsernsMode
	var created struct {
		ID string `json:"Id"`
	}
	if err := call("POST", "/containers/create", map[string]any{
		"Image": p.self.Image, "User": "0", "Entrypoint": entrypoint, "Cmd": cmd, "HostConfig": host,
	}, &created); err != nil {
		return 0, err
	}
	// The wait starts before the container, as in the docker CLI, so that the removal cannot come
	// first. The daemon answers with the headers once the wait is in place.
	wait, err := client.Post("http://docker/containers/"+created.ID+"/wait?condition=removed", "", nil)
	if err == nil {
		defer wait.Body.Close()
		if wait.StatusCode != http.StatusOK {
			err = fmt.Errorf("wait: %s", wait.Status)
		}
	}
	if err == nil {
		err = call("POST", "/containers/"+created.ID+"/start", nil, nil)
	}
	if err != nil {
		_ = call("DELETE", "/containers/"+created.ID+"?force=1", nil, nil)
		return 0, err
	}
	var result struct{ StatusCode int }
	if err := json.NewDecoder(wait.Body).Decode(&result); err != nil {
		return 0, err
	}
	return result.StatusCode, nil
}

// rename runs in the helper container. Its exit code is the errno, which the daemon reports.
func rename(from, to string) (int, error) {
	err := os.Rename(from, to)
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return int(errno), err
	}
	if err != nil {
		return 1, err
	}
	return 0, nil
}
