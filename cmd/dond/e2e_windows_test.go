package main

// The end to end tests of docker-wsl-shim, run with the native docker CLI behind this wrapper. They
// need Docker in WSL, and DOND_SHIM_DOCKER_PATH set to a docker.exe of Windows. Compose and Buildx
// come from the docker CLI's usual places, or from DOCKER_CONFIG.

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

const image = "alpine"

// buildShim builds the wrapper as docker.exe, or skips the test.
func buildShim(t *testing.T) string {
	t.Helper()
	if os.Getenv(dockerPathEnv) == "" {
		t.Skip(dockerPathEnv + " does not name a docker CLI of Windows")
	}
	if err := exec.Command("wsl.exe", "-e", "docker", "info").Run(); err != nil {
		t.Skip("no Docker in the default WSL distribution")
	}
	exe := filepath.Join(t.TempDir(), "docker.exe")
	if out, err := exec.Command("go", "build", "-o", exe, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	return exe
}

func TestEndToEnd(t *testing.T) {
	docker := buildShim(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hello world.txt"), []byte("from windows"), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(t *testing.T, args ...string) string {
		t.Helper()
		cmd := exec.Command(docker, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "SHIM_TEST=a b $c")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("docker %q: %v\n%s", args, err, out)
		}
		return strings.TrimRight(string(out), "\r\n")
	}
	expect := func(t *testing.T, got, want string) {
		t.Helper()
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}

	t.Run("arguments arrive unchanged", func(t *testing.T) {
		weird := []string{"two words", `quo"te`, `back\slash`, `trail\`, "$HOME", "*", "", "a;b&c|d", "%PATH%", "{{.x}}", `C:\a b`}
		got := run(t, append([]string{"run", "--rm", image, "printf", `[%s]`}, weird...)...)
		expect(t, got, "["+strings.Join(weird, "][")+"]")
	})
	t.Run("volume", func(t *testing.T) {
		expect(t, run(t, "run", "--rm", "-v", dir+":/data:ro", image, "cat", "/data/hello world.txt"), "from windows")
	})
	t.Run("volume with forward slashes", func(t *testing.T) {
		expect(t, run(t, "run", "--rm", "-v", filepath.ToSlash(dir)+":/data", image, "cat", "/data/hello world.txt"), "from windows")
	})
	t.Run("volume with --context default", func(t *testing.T) {
		expect(t, run(t, "--context", "default", "run", "--rm", "-v", dir+":/data:ro", image, "cat", "/data/hello world.txt"), "from windows")
	})
	t.Run("mount", func(t *testing.T) {
		expect(t, run(t, "run", "--rm", "--mount", "type=bind,src="+dir+",dst=/data", image, "cat", "/data/hello world.txt"), "from windows")
	})
	t.Run("relative volume uses the working directory", func(t *testing.T) {
		expect(t, run(t, "run", "--rm", "-v", `.\:/data`, image, "cat", "/data/hello world.txt"), "from windows")
	})
	t.Run("relative mount", func(t *testing.T) {
		expect(t, run(t, "run", "--rm", "--mount", `type=bind,src=.\,dst=/data,readonly`, image, "cat", "/data/hello world.txt"), "from windows")
	})
	t.Run("environment variable by name", func(t *testing.T) {
		expect(t, run(t, "run", "--rm", "-e", "SHIM_TEST", image, "printenv", "SHIM_TEST"), "a b $c")
	})
	t.Run("cidfile", func(t *testing.T) {
		cid := filepath.Join(dir, "cid.txt")
		run(t, "run", "--rm", "--cidfile", cid, image, "true")
		if raw, err := os.ReadFile(cid); err != nil || len(raw) != 64 {
			t.Errorf("cidfile: %q %v", raw, err)
		}
	})
	t.Run("cp and exec", func(t *testing.T) {
		const name = "dond-test-cp"
		t.Cleanup(func() { _ = exec.Command(docker, "rm", "-f", name).Run() })
		run(t, "run", "-d", "--name", name, image, "sleep", "60")
		run(t, "cp", filepath.Join(dir, "hello world.txt"), name+":/in.txt")
		expect(t, run(t, "exec", name, "cat", "/in.txt"), "from windows")
		run(t, "exec", name, "sh", "-c", "echo from linux > /out.txt")
		run(t, "cp", name+":/out.txt", `.\out.txt`)
		if got, err := os.ReadFile(filepath.Join(dir, "out.txt")); err != nil || string(got) != "from linux\n" {
			t.Errorf("got %q %v", got, err)
		}
	})
	t.Run("build and compose", func(t *testing.T) {
		compose := "services:\n  app:\n    build:\n      context: .\n    image: dond-test-image\n    environment: [\"V=${SHIM_TEST}\"]\n    volumes: [\".:/data\"]\n"
		files := map[string]string{"compose.yml": compose, "Dockerfile": "FROM " + image + "\nCOPY [\"hello world.txt\", \"/copied.txt\"]\n"}
		for name, content := range files {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		t.Cleanup(func() { _ = exec.Command(docker, "rmi", "-f", "dond-test-image").Run() })
		run(t, "build", "-q", "-t", "dond-test-image", "-f", filepath.Join(dir, "Dockerfile"), dir)
		expect(t, run(t, "run", "--rm", "dond-test-image", "cat", "/copied.txt"), "from windows")
		t.Cleanup(func() {
			_ = exec.Command(docker, "compose", "-p", "dond-test", "-f", filepath.Join(dir, "compose.yml"), "down").Run()
		})
		// A relative path in the compose file, and a relative compose file.
		got := run(t, "compose", "--progress", "quiet", "-p", "dond-test", "-f", `.\compose.yml`, "run", "--rm", "-T", "app", "sh", "-c", "echo $V; cat '/data/hello world.txt'")
		if !strings.HasSuffix(got, "a b $c\nfrom windows") {
			t.Errorf("got %q", got)
		}
	})
	t.Run("exit code and separate streams", func(t *testing.T) {
		cmd := exec.Command(docker, "run", "--rm", image, "sh", "-c", "echo out; echo err >&2; exit 37")
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		_ = cmd.Run()
		if code := cmd.ProcessState.ExitCode(); code != 37 || stdout.String() != "out\n" || stderr.String() != "err\n" {
			t.Errorf("code %d, stdout %q, stderr %q", code, &stdout, &stderr)
		}
	})
	t.Run("without a console, as from a GUI", func(t *testing.T) {
		cmd := exec.Command(docker, "run", "--rm", "-i", image, "sh", "-c", "cat; exit 5")
		cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.DETACHED_PROCESS}
		cmd.Stdin = strings.NewReader("in")
		out, _ := cmd.Output()
		if code := cmd.ProcessState.ExitCode(); code != 5 || string(out) != "in" {
			t.Errorf("code %d, stdout %q", code, out)
		}
	})
	t.Run("stdin, binary safe, ends on EOF", func(t *testing.T) {
		data := make([]byte, 1<<20)
		for i := range data {
			data[i] = byte(i * 7)
		}
		cmd := exec.Command(docker, "run", "--rm", "-i", image, "cat")
		cmd.Stdin = bytes.NewReader(data)
		out, err := cmd.Output()
		if err != nil || !bytes.Equal(out, data) {
			t.Errorf("%d bytes came back, err %v", len(out), err)
		}
	})
	t.Run("output after the end of stdin", func(t *testing.T) {
		cmd := exec.Command(docker, "run", "--rm", "-i", image, "sh", "-c", "cat; sleep 2; echo ' after'")
		cmd.Stdin = strings.NewReader("in")
		if out, err := cmd.Output(); err != nil || string(out) != "in after\n" {
			t.Errorf("got %q %v", out, err)
		}
	})
	t.Run("a path that does not exist is not created on Windows", func(t *testing.T) {
		missing := filepath.Join(dir, "missing")
		cmd := exec.Command(docker, "run", "--rm", "--mount", "type=bind,src="+missing+",dst=/m", image, "true")
		if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "/mnt/") {
			t.Errorf("err %v: %s", err, out)
		}
		if _, err := os.Stat(missing); err == nil {
			t.Error("created")
		}
	})
}

// Killing docker.exe must end the client too, as it would with the real one.
func TestKilledShimEndsTheClient(t *testing.T) {
	docker := buildShim(t)
	const name = "dond-test-kill"
	t.Cleanup(func() { _ = exec.Command(docker, "rm", "-f", name).Run() })
	cmd := exec.Command(docker, "run", "--rm", "-i", "--name", name, image, "cat")
	stdin, err := cmd.StdinPipe() // held open: only the kill can end the container
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	cmd.Stdout = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	running := func() bool {
		out, _ := exec.Command(docker, "ps", "-q", "--filter", "name="+name).Output()
		return len(bytes.TrimSpace(out)) > 0
	}
	wait := func(want bool) bool {
		for range 40 {
			if running() == want {
				return true
			}
			time.Sleep(250 * time.Millisecond)
		}
		return false
	}
	if !wait(true) {
		t.Fatal("the container did not start")
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	if !wait(false) {
		t.Error("the container outlived the killed docker.exe")
	}
}

// dond daemon serves the default pipe, for clients of Windows with no wrapper.
func TestDaemon(t *testing.T) {
	shim := buildShim(t)
	if pipeExists(enginePipe) {
		t.Skip(enginePipe + " exists already")
	}
	dond := filepath.Join(filepath.Dir(shim), "dond.exe")
	if err := os.Rename(shim, dond); err != nil {
		t.Fatal(err)
	}
	start := func() (*exec.Cmd, *output) {
		cmd, out := exec.Command(dond, "daemon"), &output{}
		cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
		cmd.Stdout, cmd.Stderr = out, out
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill() })
		return cmd, out
	}
	daemon, out := start()
	for i := 0; !strings.Contains(out.String(), "serving at"); i++ {
		if i == 100 {
			t.Fatalf("did not start: %q", out)
		}
		time.Sleep(50 * time.Millisecond)
	}

	second, secondOut := start()
	_ = second.Wait()
	if code := second.ProcessState.ExitCode(); code != 1 || !strings.Contains(secondOut.String(), "cannot serve") {
		t.Errorf("a second daemon: exit code %d, %q", code, secondOut)
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("through the daemon"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Getenv(dockerPathEnv), "run", "--rm", "-v", dir+":/d", image, "cat", "/d/f.txt")
	cmd.Env = append(os.Environ(), "DOCKER_HOST=")
	if got, err := cmd.CombinedOutput(); err != nil || string(got) != "through the daemon" {
		t.Errorf("got %q %v", got, err)
	}

	if err := windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(daemon.Process.Pid)); err != nil {
		t.Fatal(err)
	}
	_ = daemon.Wait()
	if code := daemon.ProcessState.ExitCode(); code != 0 {
		t.Errorf("exit code %d, %q", code, out)
	}
	if pipeExists(enginePipe) {
		t.Error(enginePipe + " is still there")
	}
}
