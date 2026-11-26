package main

import (
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

const (
	trapInterrupt = `trap 'echo GO"T"INT; exit 42' INT; echo RE"A"DY; while :; do sleep 0.1; done`
	middlemanEnv  = "DOND_SHIM_TEST_MIDDLEMAN"
)

// TestMain can also run a command with pipes instead of a console and outlive Ctrl+C, as a program
// that uses docker does.
func TestMain(m *testing.M) {
	// Ignoring Ctrl+C is inherited, and these tests may have been started that way: then no process
	// in the terminals of the tests would get it.
	_, _, _ = windows.NewLazySystemDLL("kernel32.dll").NewProc("SetConsoleCtrlHandler").Call(0, 0)
	if os.Getenv(middlemanEnv) == "" {
		os.Exit(m.Run())
	}
	signal.Notify(make(chan os.Signal, 1), os.Interrupt)
	cmd := exec.Command(os.Args[1], os.Args[2:]...)
	stdin, _ := cmd.StdinPipe() // held open
	if os.Getenv(middlemanEnv) == "stdin" {
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		_ = cmd.Run()
		os.Exit(cmd.ProcessState.ExitCode())
	}
	stdout, _ := cmd.StdoutPipe()
	cmd.Stderr = cmd.Stdout
	_ = cmd.Start()
	buffer := make([]byte, 4096)
	for {
		n, err := stdout.Read(buffer)
		_, _ = os.Stdout.Write(buffer[:n])
		if err != nil {
			break
		}
	}
	_ = cmd.Wait()
	_ = stdin.Close()
	os.Exit(cmd.ProcessState.ExitCode())
}

// output is written to by a command and read by the test.
type output struct {
	mu   sync.Mutex
	text strings.Builder
}

func (o *output) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.text.Write(p)
}

func (o *output) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.text.String()
}

func TestInterrupt(t *testing.T) {
	docker := buildShim(t)

	// start runs docker in a group of its own, as a program that wants to interrupt it does.
	start := func(t *testing.T, args ...string) (*exec.Cmd, *output) {
		t.Helper()
		cmd, out := exec.Command(docker, args...), &output{}
		cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
		cmd.Stdout, cmd.Stderr = out, out
		stdin, err := cmd.StdinPipe() // held open
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = stdin.Close()
			_ = cmd.Process.Kill()
		})
		return cmd, out
	}
	ctrlBreak := func(t *testing.T, cmd *exec.Cmd) {
		t.Helper()
		if err := windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(cmd.Process.Pid)); err != nil {
			t.Fatal(err)
		}
	}
	expect := func(t *testing.T, out *output, want string) {
		t.Helper()
		for range 120 {
			if strings.Contains(out.String(), want) {
				return
			}
			time.Sleep(250 * time.Millisecond)
		}
		t.Fatalf("%q did not appear in %q", want, out)
	}
	expectExit := func(t *testing.T, cmd *exec.Cmd, out *output, want int) {
		t.Helper()
		timer := time.AfterFunc(30*time.Second, func() { _ = cmd.Process.Kill() })
		defer timer.Stop()
		_ = cmd.Wait()
		if code := cmd.ProcessState.ExitCode(); code != want {
			t.Errorf("exit code %d, want %d, %q", code, want, out)
		}
	}

	for _, flag := range []string{"-i", "--sig-proxy=true"} {
		t.Run("Ctrl+Break reaches the container "+flag, func(t *testing.T) {
			cmd, out := start(t, "run", "--rm", "--init", flag, image, "sh", "-c", trapInterrupt)
			expect(t, out, "READY")
			ctrlBreak(t, cmd)
			expectExit(t, cmd, out, 42)
			if strings.Count(out.String(), "GOTINT") != 1 {
				t.Errorf("got %q", out)
			}
		})
	}
	t.Run("twice, the first one is ignored", func(t *testing.T) {
		cmd, out := start(t, "run", "--rm", "--init", image, "sh", "-c",
			`n=0; trap 'n=$((n+1)); echo INT$n; [ $n = 2 ] && exit 9' INT; echo RE"A"DY; while :; do sleep 0.1; done`)
		expect(t, out, "READY")
		ctrlBreak(t, cmd)
		expect(t, out, "INT1")
		ctrlBreak(t, cmd)
		expectExit(t, cmd, out, 9)
	})
	t.Run("only the client of this shim", func(t *testing.T) {
		first, firstOut := start(t, "run", "--rm", "--init", image, "sh", "-c", trapInterrupt)
		second, secondOut := start(t, "run", "--rm", "--init", image, "sh", "-c", trapInterrupt)
		expect(t, firstOut, "READY")
		expect(t, secondOut, "READY")
		ctrlBreak(t, first)
		expectExit(t, first, firstOut, 42)
		time.Sleep(time.Second)
		if strings.Contains(secondOut.String(), "GOTINT") {
			t.Error("the other container got it too")
		}
		ctrlBreak(t, second)
		expectExit(t, second, secondOut, 42)
	})
	t.Run("right after the start", func(t *testing.T) {
		// docker itself loses an interrupt at times while it starts the container. Like a person, try again.
		for _, delay := range []time.Duration{50, 100, 150, 250, 400} {
			// docker can leave the container behind, created but not started.
			const name = "dond-test-early"
			_ = exec.Command(docker, "rm", "-f", name).Run()
			t.Cleanup(func() { _ = exec.Command(docker, "rm", "-f", name).Run() })
			cmd, out := start(t, "run", "--rm", "--init", "--name", name, image, "sleep", "100")
			time.Sleep(delay * time.Millisecond)
			ctrlBreak(t, cmd)
			began := time.Now()
			again := time.AfterFunc(3*time.Second, func() { ctrlBreak(t, cmd) })
			timer := time.AfterFunc(20*time.Second, func() {
				t.Errorf("after %d ms: it did not end: %q", delay, out)
				_ = cmd.Process.Kill()
			})
			_ = cmd.Wait()
			timer.Stop()
			again.Stop()
			t.Logf("after %d ms: exit code %#x in %v", delay, uint32(cmd.ProcessState.ExitCode()), time.Since(began).Round(time.Millisecond))
		}
	})
	t.Run("compose up stops its containers", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "compose.yml")
		if err := os.WriteFile(file, []byte("services:\n  app:\n    image: "+image+"\n    init: true\n    command: [sh, -c, 'echo READY; exec sleep 100']\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		const project = "dond-test-interrupt"
		t.Cleanup(func() { _ = exec.Command(docker, "compose", "-p", project, "-f", file, "down").Run() })
		cmd, out := start(t, "compose", "-p", project, "-f", file, "up")
		expect(t, out, "READY")
		ctrlBreak(t, cmd)
		expectExit(t, cmd, out, 130)
		if left, err := exec.Command(docker, "compose", "-p", project, "-f", file, "ps", "-q").Output(); err != nil || len(left) > 0 {
			t.Errorf("still running: %q %v", left, err)
		}
	})
	for _, piped := range []string{"all", "stdin"} {
		t.Run("Ctrl+C in the terminal of a program that uses docker, pipes: "+piped, func(t *testing.T) {
			t.Setenv(middlemanEnv, piped)
			term := startInTerminal(t, os.Args[0], docker, "run", "--rm", "--init", "-i", image, "sh", "-c", trapInterrupt)
			term.expect("READY")
			term.write("\x03")
			term.expect("GOTINT")
			if code := term.exitCode(); code != 42 {
				t.Errorf("exit code %d, want 42", code)
			}
		})
	}
}
