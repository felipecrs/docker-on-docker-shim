package main

import (
	"context"
	"net"
	"os/exec"
	"testing"
	"time"
)

func TestDialSkipsSparesThatEnded(t *testing.T) {
	dead, err := startStdio(exec.Command("cmd.exe", "/c", "exit 0"))
	if err != nil {
		t.Fatal(err)
	}
	for start := time.Now(); !dead.exited(); time.Sleep(10 * time.Millisecond) {
		if time.Since(start) > 10*time.Second {
			t.Fatal("cmd.exe did not end")
		}
	}
	var started []*stdioConn
	tr := &transport{firstDone: make(chan struct{}), spares: make(chan spare, spares), probedAt: time.Now()}
	close(tr.firstDone)
	tr.dialStdio = func() (net.Conn, error) {
		// more waits for its input, as dial-stdio does.
		c, err := startStdio(exec.Command("cmd.exe", "/c", "more"))
		if err == nil {
			tr.mu.Lock()
			started = append(started, c)
			tr.mu.Unlock()
		}
		return c, err
	}
	t.Cleanup(func() {
		tr.stopSpares()
		tr.mu.Lock()
		defer tr.mu.Unlock()
		for _, c := range started {
			c.Close()
		}
	})
	// The dead one waits first, as a spare whose wsl.exe ended with WSL.
	tr.sparing, tr.stop = true, make(chan struct{})
	tr.spares <- spare{dead, nil}
	go tr.addSpare(tr.epoch)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := tr.dial(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if conn == net.Conn(dead) {
		t.Fatal("got the spare whose process ended")
	}
	if conn.(*stdioConn).exited() {
		t.Fatal("got a spare whose process ended")
	}
}
