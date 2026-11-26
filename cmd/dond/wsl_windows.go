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
	// vsockTimeout bounds a connection to the listener once found, which can go away with WSL.
	vsockTimeout = time.Second
	// reprobeAfter is how long a long-running proxy, such as dond daemon, goes without the listener
	// before it looks for it again.
	reprobeAfter = 30 * time.Second
	// spareDelay is how long the probe may take before wsl.exe starts, as it is the slow part.
	spareDelay = 10 * time.Millisecond
	// spares is how many dial-stdio wait ahead. docker run uses three connections, but a third one
	// started with the others slows down the commands that use one.
	spares = 2
)

// transport reaches the Docker daemon in WSL, through a Hyper-V socket when a listener in WSL
// answers on vsockPort, or else through `docker system dial-stdio` in WSL for each connection.
type transport struct {
	start     time.Time
	firstDone chan struct{}            // closed when the first probe ended
	spares    chan spare               // dial-stdio started ahead of the need, as wsl.exe takes 100 ms to start
	dialStdio func() (net.Conn, error) // starts a dial-stdio

	mu       sync.Mutex
	vm       *guid.GUID // the VM with the listener, or nil
	probedAt time.Time
	probing  bool
	sparing  bool
	stop     chan struct{} // closed when the spares stop
	found    []func()      // called when a later probe finds the listener
	epoch    int           // counts the starts of spares, so that a spare of an earlier start is dropped
}

type spare struct {
	conn net.Conn
	err  error
}

// wslTransport starts to reach WSL at its first call.
var wslTransport = sync.OnceValue(func() *transport {
	t := newTransport(dialStdio)
	go func() {
		select {
		case <-t.firstDone:
		case <-time.After(spareDelay):
			t.startSpares()
			<-t.firstDone
		}
		if t.listener() == nil {
			t.startSpares()
		} else {
			t.stopSpares()
		}
	}()
	return t
})

func newTransport(dialStdio func() (net.Conn, error)) *transport {
	t := &transport{start: time.Now(), firstDone: make(chan struct{}), spares: make(chan spare, spares), dialStdio: dialStdio}
	t.probe(t.firstDone)
	return t
}

func dialUpstream(ctx context.Context, network, address string) (net.Conn, error) {
	if network != "wsl" {
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	return wslTransport().dial(ctx)
}

func (t *transport) dial(ctx context.Context) (net.Conn, error) {
	select {
	case <-t.firstDone:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if vm := t.listener(); vm != nil {
		conn, err := dialVsock(*vm, vsockTimeout)
		if err == nil {
			return conn, nil
		}
		debugf("listener on vsock port %d of VM %s: %v, using wsl.exe", vsockPort, vm, err)
		t.mu.Lock()
		if t.vm != nil && *t.vm == *vm {
			t.vm = nil
		}
		t.mu.Unlock()
		// WSL may have restarted, with a new VM.
		t.probe(nil)
	} else if t.stale() {
		t.probe(nil)
	}
	stopped, epoch := t.startSpares()
	for {
		select {
		case s := <-t.spares:
			go t.addSpare(epoch)
			if c, ok := s.conn.(*stdioConn); ok && c.exited() {
				debugf("wsl.exe of a spare connection ended, taking another")
				_ = c.Close()
				continue
			}
			debugf("connection through wsl.exe %v after the start", time.Since(t.start))
			return s.conn, s.err
		case <-stopped:
			// A probe found the listener meanwhile.
			return t.dial(ctx)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// onListenerFound calls f each time a probe after the first one finds the listener.
func onListenerFound(f func()) {
	t := wslTransport()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.found = append(t.found, f)
}

func (t *transport) listener() *guid.GUID {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.vm
}

func (t *transport) stale() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return !t.probing && time.Since(t.probedAt) > reprobeAfter
}

func vsockAddr(vm guid.GUID) *winio.HvsockAddr {
	return &winio.HvsockAddr{VMID: vm, ServiceID: winio.VsockServiceID(vsockPort)}
}

// dialVsock connects to the listener in vm, or gives up after timeout, as go-winio ignores the
// context.
func dialVsock(vm guid.GUID, timeout time.Duration) (net.Conn, error) {
	done := make(chan spare, 1)
	go func() {
		conn, err := winio.Dial(context.Background(), vsockAddr(vm))
		done <- spare{conn, err}
	}()
	select {
	case s := <-done:
		if s.err != nil {
			return nil, s.err
		}
		return s.conn, nil
	case <-time.After(timeout):
		go func() {
			// go-winio returns a nil *HvsockConn with its errors.
			if s := <-done; s.err == nil {
				s.conn.Close()
			}
		}()
		return nil, fmt.Errorf("no answer after %v", timeout)
	}
}

// probe looks for the listener in every running VM in the background, as the ID of the VM of WSL is
// known only to the system, and closes done when it ends. It does nothing while a probe runs.
func (t *transport) probe(done chan struct{}) {
	t.mu.Lock()
	if t.probing {
		t.mu.Unlock()
		return
	}
	t.probing = true
	t.mu.Unlock()
	go func() {
		vm := findListener()
		t.mu.Lock()
		t.vm, t.probedAt, t.probing = vm, time.Now(), false
		t.mu.Unlock()
		if vm == nil {
			debugf("no listener on vsock port %d after %v, using wsl.exe", vsockPort, time.Since(t.start))
		} else {
			debugf("listener on vsock port %d of VM %s after %v", vsockPort, vm, time.Since(t.start))
			// A long-running proxy that found the listener again needs no wsl.exe.
			if done == nil {
				t.stopSpares()
				t.mu.Lock()
				found := t.found
				t.mu.Unlock()
				for _, f := range found {
					f()
				}
			}
		}
		if done != nil {
			close(done)
		}
	}()
}

func findListener() *guid.GUID {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows NT\CurrentVersion\HostComputeService\VolatileStore\ComputeSystem`, registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		return nil
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
			if conn, err := dialVsock(vm, probeTimeout); err == nil {
				conn.Close()
				found <- vm
			}
		}()
	}
	if len(names) == 0 {
		return nil
	}
	select {
	case vm := <-found:
		return &vm
	case <-time.After(probeTimeout):
		return nil
	}
}

// startSpares keeps spares connections waiting, unless it does already. It returns a channel closed
// when they stop, and the epoch for addSpare.
func (t *transport) startSpares() (chan struct{}, int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.sparing {
		t.sparing, t.stop = true, make(chan struct{})
		t.epoch++
		for range spares {
			go t.addSpare(t.epoch)
		}
	}
	return t.stop, t.epoch
}

// stopSpares ends the spares, and the ones still starting once they started.
func (t *transport) stopSpares() {
	t.mu.Lock()
	if t.sparing {
		t.sparing = false
		close(t.stop)
	}
	t.mu.Unlock()
	for {
		select {
		case s := <-t.spares:
			if s.conn != nil {
				s.conn.Close()
			}
		default:
			return
		}
	}
}

func (t *transport) addSpare(epoch int) {
	conn, err := t.dialStdio()
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.sparing && t.epoch == epoch {
		select {
		case t.spares <- spare{conn, err}:
			return
		default:
		}
	}
	if conn != nil {
		conn.Close()
	}
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
	c, err := startStdio(cmd)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// startStdio starts cmd as a connection through its standard streams.
func startStdio(cmd *exec.Cmd) (*stdioConn, error) {
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

// exited tells whether wsl.exe ended, as when WSL stops. The pid stays valid: os.Process keeps a
// handle to the process until Wait.
func (c *stdioConn) exited() bool {
	process, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(c.cmd.Process.Pid))
	if err != nil {
		return true
	}
	defer windows.CloseHandle(process)
	event, err := windows.WaitForSingleObject(process, 0)
	return err == nil && event == windows.WAIT_OBJECT_0
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
