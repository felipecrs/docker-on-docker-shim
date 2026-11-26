package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Microsoft/go-winio"
	"github.com/Microsoft/go-winio/pkg/guid"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const (
	// distroEnv names the distribution of WSL to use instead of the default one.
	distroEnv = "DOND_SHIM_WSL_DISTRO"
	// vsockPort is where the optional listener in WSL serves the Docker socket.
	vsockPort = 2375
	// probeTimeout bounds the wait for the listener: a connection to a port nobody listens on is not
	// refused, it times out after 30 seconds. A connection takes about a millisecond.
	probeTimeout = 50 * time.Millisecond
	// spareDelay is how long the probe may take before wsl.exe starts, as it is the slow part.
	spareDelay = 10 * time.Millisecond
	// spares is how many dial-stdio wait ahead. docker run uses three connections, but a third one
	// started with the others slows down the commands that use one.
	spares = 2
)

// transport reaches the Docker daemon in WSL, through a Hyper-V socket when a listener in WSL
// answers on vsockPort, or else through `docker system dial-stdio` in WSL for each connection.
type transport struct {
	probed chan struct{} // closed when vm is known
	vm     *guid.GUID    // the VM with the listener, or nil
	spares chan spare    // dial-stdio started ahead of the need, as wsl.exe takes 100 ms to start
	start  time.Time
}

type spare struct {
	conn net.Conn
	err  error
}

// wslTransport starts to reach WSL at its first call.
var wslTransport = sync.OnceValue(func() *transport {
	t := &transport{probed: make(chan struct{}), spares: make(chan spare, spares), start: time.Now()}
	go t.probe()
	go func() {
		select {
		case <-t.probed:
			if t.vm == nil {
				t.addSpares()
			}
		case <-time.After(spareDelay):
			t.addSpares()
			if <-t.probed; t.vm != nil {
				for range spares {
					if s := <-t.spares; s.conn != nil {
						s.conn.Close()
					}
				}
			}
		}
	}()
	return t
})

func dialUpstream(ctx context.Context, network, address string) (net.Conn, error) {
	if network != "wsl" {
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	return wslTransport().dial(ctx)
}

func (t *transport) dial(ctx context.Context) (net.Conn, error) {
	select {
	case <-t.probed:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if t.vm != nil {
		return winio.Dial(ctx, vsockAddr(*t.vm))
	}
	select {
	case s := <-t.spares:
		debugf("connection through wsl.exe %v after the start", time.Since(t.start))
		go t.addSpare()
		return s.conn, s.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func vsockAddr(vm guid.GUID) *winio.HvsockAddr {
	return &winio.HvsockAddr{VMID: vm, ServiceID: winio.VsockServiceID(vsockPort)}
}

// probe looks for the listener in every running VM, as the ID of the VM of WSL is known only to
// the system.
func (t *transport) probe() {
	defer func() {
		if t.vm == nil {
			debugf("no listener on vsock port %d after %v, using wsl.exe", vsockPort, time.Since(t.start))
		} else {
			debugf("listener on vsock port %d of VM %s after %v", vsockPort, t.vm, time.Since(t.start))
		}
		close(t.probed)
	}()
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows NT\CurrentVersion\HostComputeService\VolatileStore\ComputeSystem`, registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		return
	}
	names, _ := key.ReadSubKeyNames(-1)
	key.Close()
	found := make(chan guid.GUID, len(names))
	for _, name := range names {
		vm, err := guid.FromString(name)
		if err != nil {
			continue
		}
		go func() {
			if conn, err := winio.Dial(context.Background(), vsockAddr(vm)); err == nil {
				conn.Close()
				found <- vm
			}
		}()
	}
	if len(names) == 0 {
		return
	}
	select {
	case vm := <-found:
		t.vm = &vm
	case <-time.After(probeTimeout):
	}
}

func (t *transport) addSpares() {
	for range spares {
		go t.addSpare()
	}
}

func (t *transport) addSpare() {
	conn, err := dialStdio()
	t.spares <- spare{conn, err}
}

// dialStdio starts `docker system dial-stdio` in the distribution of WSL.
func dialStdio() (net.Conn, error) {
	exe := filepath.Join(os.Getenv("SystemRoot"), "System32", "wsl.exe")
	// The one in System32 only starts this one, which takes time.
	if direct := filepath.Join(os.Getenv("ProgramFiles"), "WSL", "wsl.exe"); fileExists(direct) {
		exe = direct
	}
	args := []string{"-e", "docker", "system", "dial-stdio"}
	if distro := os.Getenv(distroEnv); distro != "" {
		args = append([]string{"-d", distro}, args...)
	}
	cmd := exec.Command(exe, args...)
	// wsl.exe prints its own errors, such as an unknown distribution, in UTF-16 otherwise.
	cmd.Env = append(os.Environ(), "WSL_UTF8=1")
	// Without a console, Ctrl+C in the console of the command does not end the connection.
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.DETACHED_PROCESS}
	c := &stdioConn{cmd: cmd}
	cmd.Stderr = &c.stderr
	var err error
	if c.stdin, err = cmd.StdinPipe(); err != nil {
		return nil, err
	}
	if c.stdout, err = cmd.StdoutPipe(); err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return c, nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// stdioConn is a connection to the Docker daemon through the standard streams of wsl.exe.
type stdioConn struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr bytes.Buffer
	read   bool
	// answered is set once wsl.exe gave an answer of HTTP, and not an error of its own.
	answered atomic.Bool

	failure    sync.Once
	failureErr error
}

func (c *stdioConn) Read(b []byte) (int, error) {
	n, err := c.stdout.Read(b)
	first := !c.read
	c.read = c.read || n > 0
	if first && (n > 0 && !strings.HasPrefix("HTTP/", string(b[:min(n, 5)])) || errors.Is(err, io.EOF)) {
		return 0, c.failed(b[:n:n])
	}
	if first && n > 0 {
		c.answered.Store(true)
	}
	return n, err
}

func (c *stdioConn) Write(b []byte) (int, error) {
	n, err := c.stdin.Write(b)
	if err != nil && !c.answered.Load() {
		return n, c.failed(nil)
	}
	return n, err
}

// failed says why wsl.exe ended before it answered. wsl.exe prints its own errors on the output,
// and docker on the error output.
func (c *stdioConn) failed(output []byte) error {
	c.failure.Do(func() {
		rest, _ := io.ReadAll(c.stdout)
		_ = c.cmd.Wait()
		message := bytes.TrimSpace(append(append(output, rest...), c.stderr.Bytes()...))
		c.failureErr = fmt.Errorf("%s: %v: %s", strings.Join(c.cmd.Args, " "), c.cmd.ProcessState, message)
	})
	return c.failureErr
}

// CloseWrite ends the input, as an attach whose input ended does.
func (c *stdioConn) CloseWrite() error { return c.stdin.Close() }

func (c *stdioConn) Close() error {
	_ = c.stdin.Close()
	_ = c.cmd.Process.Kill()
	go c.cmd.Wait()
	return nil
}

type wslAddr struct{}

func (wslAddr) Network() string { return "wsl" }
func (wslAddr) String() string  { return "wsl" }

func (c *stdioConn) LocalAddr() net.Addr                { return wslAddr{} }
func (c *stdioConn) RemoteAddr() net.Addr               { return wslAddr{} }
func (c *stdioConn) SetDeadline(t time.Time) error      { return nil }
func (c *stdioConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *stdioConn) SetWriteDeadline(t time.Time) error { return nil }

// wslRoot returns where the drives of Windows are in the distribution, read once from its wsl.conf
// when the first path of Windows is mapped.
func wslRoot() func() string {
	return sync.OnceValue(func() string {
		name, err := os.Getenv(distroEnv), error(nil)
		if name == "" {
			name, err = defaultDistribution()
		}
		if err != nil {
			debugf("default distribution: %v, drives taken under /mnt/", err)
			return "/mnt/"
		}
		conf, err := os.ReadFile(`\\wsl.localhost\` + name + `\etc\wsl.conf`)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			debugf("wsl.conf of %s: %v, drives taken under /mnt/", name, err)
		}
		root := automountRoot(string(conf))
		debugf("drives under %s in %s", root, name)
		return root
	})
}

func defaultDistribution() (string, error) {
	const lxss = `Software\Microsoft\Windows\CurrentVersion\Lxss`
	key, err := registry.OpenKey(registry.CURRENT_USER, lxss, registry.QUERY_VALUE)
	if err != nil {
		return "", err
	}
	id, _, err := key.GetStringValue("DefaultDistribution")
	key.Close()
	if err != nil {
		return "", err
	}
	if key, err = registry.OpenKey(registry.CURRENT_USER, lxss+`\`+id, registry.QUERY_VALUE); err != nil {
		return "", err
	}
	defer key.Close()
	name, _, err := key.GetStringValue("DistributionName")
	return name, err
}
