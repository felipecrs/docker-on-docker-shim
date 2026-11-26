package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"unsafe"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

const defaultHost = "npipe:////./pipe/docker_engine"

// execCommand runs the command without a proxy. Windows cannot replace a process with another.
func execCommand(path, name string, args []string) (int, error) {
	cmd := exec.Command(path, args...)
	cmd.Args[0] = name
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return runCommand(cmd)
}

func selfPath() string {
	path, _ := os.Executable()
	return path
}

// listenPrivate listens on a named pipe with a random name, which only the current user may open.
func listenPrivate() (listener net.Listener, host, socket string, err error) {
	random := make([]byte, 16)
	_, _ = rand.Read(random)
	name := "dond-shim-" + hex.EncodeToString(random)
	if listener, err = listenPipe(`\\.\pipe\` + name); err != nil {
		return nil, "", "", err
	}
	// Start reaching WSL now, while the command starts.
	wslTransport()
	return listener, "npipe:////./pipe/" + name, "", nil
}

// listenPipe listens on a new named pipe that only the current user may open. It fails when the pipe
// exists already.
func listenPipe(path string) (net.Listener, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	return winio.ListenPipe(path, &winio.PipeConfig{
		// P: no inherited entries, so the DACL has the current user alone.
		SecurityDescriptor: "D:P(A;;GA;;;" + user.User.Sid.String() + ")",
		// As dockerd's pipe: the docker CLI closes its side of an attach with an empty message.
		MessageMode:      true,
		InputBufferSize:  65536,
		OutputBufferSize: 65536,
	})
}

// runCommand runs the command and returns its exit code. Ctrl+C and Ctrl+Break reach the command
// from the console by themselves, so this process only outlives them.
func runCommand(cmd *exec.Cmd) (int, error) {
	signal.Notify(make(chan os.Signal, 1), os.Interrupt, syscall.SIGTERM)
	if err := killChildrenWithMe(); err != nil {
		debugf("%v", err)
	}
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), nil
	}
	if err != nil {
		return 127, err
	}
	return 0, nil
}

// killChildrenWithMe makes Windows end the processes that this process starts when it ends, however
// it ends, as the command and the connections to WSL depend on it. The job handle is left open on
// purpose: closing it is what ends them.
func killChildrenWithMe() error {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return fmt.Errorf("create job object: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		return fmt.Errorf("set up job object: %w", err)
	}
	if err := windows.AssignProcessToJobObject(job, windows.CurrentProcess()); err != nil {
		return fmt.Errorf("assign to job object: %w", err)
	}
	return nil
}
