package main

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// terminal is a process in a Windows pseudo console, as in Windows Terminal.
type terminal struct {
	t       *testing.T
	console windows.Handle
	process windows.Handle
	input   *os.File
	mu      sync.Mutex
	output  bytes.Buffer
}

func startInTerminal(t *testing.T, args ...string) *terminal {
	t.Helper()
	check := func(err error) {
		if err != nil {
			t.Helper()
			t.Fatal(err)
		}
	}
	inRead, inWrite, err := os.Pipe()
	check(err)
	outRead, outWrite, err := os.Pipe()
	check(err)
	term := &terminal{t: t, input: inWrite}
	check(windows.CreatePseudoConsole(windows.Coord{X: 100, Y: 30}, windows.Handle(inRead.Fd()), windows.Handle(outWrite.Fd()), 0, &term.console))
	_ = inRead.Close()
	_ = outWrite.Close()
	attributes, err := windows.NewProcThreadAttributeList(1)
	check(err)
	defer attributes.Delete()
	// The attribute's value is the handle itself, not a pointer to it.
	check(attributes.Update(windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE, *(*unsafe.Pointer)(unsafe.Pointer(&term.console)), unsafe.Sizeof(term.console)))
	startup := windows.StartupInfoEx{ProcThreadAttributeList: attributes.List()}
	startup.Cb = uint32(unsafe.Sizeof(startup))
	startup.Flags = windows.STARTF_USESTDHANDLES // or the process would get this test's own, redirected ones
	commandLine, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(args))
	check(err)
	var process windows.ProcessInformation
	check(windows.CreateProcess(nil, commandLine, nil, nil, false, windows.EXTENDED_STARTUPINFO_PRESENT, nil, nil, &startup.StartupInfo, &process))
	_ = windows.CloseHandle(process.Thread)
	term.process = process.Process
	go func() {
		buffer := make([]byte, 4096)
		for {
			n, err := outRead.Read(buffer)
			term.mu.Lock()
			term.output.Write(buffer[:n])
			term.mu.Unlock()
			// The console asks where the cursor is and waits for the answer.
			if bytes.Contains(buffer[:n], []byte("\x1b[6n")) {
				_, _ = inWrite.WriteString("\x1b[1;1R")
			}
			if err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		_ = windows.TerminateProcess(term.process, 1)
		windows.ClosePseudoConsole(term.console)
		_ = inWrite.Close()
		_ = windows.CloseHandle(term.process)
	})
	return term
}

func (term *terminal) screen() string {
	term.mu.Lock()
	defer term.mu.Unlock()
	return term.output.String()
}

func (term *terminal) write(s string) {
	term.t.Helper()
	if _, err := term.input.WriteString(s); err != nil {
		term.t.Fatal(err)
	}
}

func (term *terminal) expect(want string) {
	term.t.Helper()
	for range 120 {
		if strings.Contains(term.screen(), want) {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	term.t.Fatalf("%q did not appear in %q", want, term.screen())
}

func (term *terminal) exitCode() int {
	term.t.Helper()
	if event, err := windows.WaitForSingleObject(term.process, 30_000); err != nil || event != windows.WAIT_OBJECT_0 {
		term.t.Fatalf("did not exit: %q", term.screen())
	}
	var code uint32
	if err := windows.GetExitCodeProcess(term.process, &code); err != nil {
		term.t.Fatal(err)
	}
	return int(code)
}

func TestTerminal(t *testing.T) {
	docker := buildShim(t)
	// The marker is assembled in the container, so that the echo of what is typed does not match.
	ready := `echo RE"A"DY; `

	t.Run("interactive shell", func(t *testing.T) {
		term := startInTerminal(t, docker, "run", "--rm", "-it", image, "sh")
		term.expect("/ #")
		// The shell drops what arrives with the first resize, which can come just after the prompt
		// through wsl.exe. A person does not type that fast.
		time.Sleep(500 * time.Millisecond)
		term.write("test -t 0 && test -t 1 && echo T\"T\"Y\r")
		term.expect("TTY")
		term.write("stty size\r")
		term.expect("30 100")
		if err := windows.ResizePseudoConsole(term.console, windows.Coord{X: 90, Y: 25}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Second)
		term.write("stty size\r")
		term.expect("25 90")
		term.write("sleep 60\r")
		time.Sleep(time.Second)
		term.write("\x03") // ends sleep, not the shell
		term.write("echo AF\"T\"ER; exit 7\r")
		term.expect("AFTER")
		if code := term.exitCode(); code != 7 {
			t.Errorf("exit code %d, want 7", code)
		}
	})
	t.Run("Ctrl+C with -it", func(t *testing.T) {
		const name = "dond-test-ctrl-c"
		t.Cleanup(func() { _ = exec.Command(docker, "rm", "-f", name).Run() })
		term := startInTerminal(t, docker, "run", "--rm", "-it", "--name", name, image, "sh", "-c", ready+"exec sleep 60")
		term.expect("READY")
		term.write("\x03")
		time.Sleep(time.Second)
		term.write("\x03")
		// Without --init, sleep is PID 1 and ignores SIGINT, as with the real CLI.
		if event, _ := windows.WaitForSingleObject(term.process, 2000); event == windows.WAIT_OBJECT_0 {
			t.Errorf("exited with %d", term.exitCode())
		}
	})
	for _, flag := range []string{"-it", "-i", "-t", "--sig-proxy=true"} {
		t.Run("Ctrl+C with --init "+flag, func(t *testing.T) {
			term := startInTerminal(t, docker, "run", "--rm", "--init", flag, image, "sh", "-c", `trap 'echo GO"T"INT; exit 42' INT; `+ready+`while :; do sleep 0.2; done`)
			term.expect("READY")
			term.write("\x03")
			term.expect("GOTINT")
			if code := term.exitCode(); code != 42 {
				t.Errorf("exit code %d, want 42", code)
			}
		})
	}
	t.Run("Ctrl+C ends events", func(t *testing.T) {
		term := startInTerminal(t, docker, "events")
		time.Sleep(2 * time.Second)
		term.write("\x03")
		if code := term.exitCode(); code != 130 {
			t.Errorf("exit code %d, want 130", code)
		}
	})
	t.Run("Ctrl+C ends logs --follow, not the container", func(t *testing.T) {
		const name = "dond-test-logs"
		if out, err := exec.Command(docker, "run", "-d", "--rm", "--init", "--name", name, image, "sh", "-c", ready+"exec sleep 60").CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		t.Cleanup(func() { _ = exec.Command(docker, "rm", "-f", name).Run() })
		term := startInTerminal(t, docker, "logs", "--follow", name)
		term.expect("READY")
		term.write("\x03")
		term.exitCode() // docker's own code varies here
		if out, err := exec.Command(docker, "inspect", "-f", "{{.State.Running}}", name).Output(); err != nil || strings.TrimSpace(string(out)) != "true" {
			t.Errorf("running: %q %v", out, err)
		}
	})
	t.Run("attach with detach keys", func(t *testing.T) {
		const name = "dond-test-detach"
		t.Cleanup(func() { _ = exec.Command(docker, "rm", "-f", name).Run() })
		term := startInTerminal(t, docker, "run", "--rm", "-it", "--name", name, image, "sh")
		term.expect("/ #")
		term.write("\x10\x11") // Ctrl+P Ctrl+Q
		if code := term.exitCode(); code != 0 {
			t.Errorf("exit code %d", code)
		}
	})
}
