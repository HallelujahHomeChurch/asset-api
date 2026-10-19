package main

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestRetentionMainDrainsInterruptedBootstrap(t *testing.T) {
	if os.Getenv("HHC_RETENTION_SIGNAL_CHILD") == "1" {
		main()
		return
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		if conn, err := listener.Accept(); err == nil {
			accepted <- conn
		}
	}()
	cmd := exec.Command(os.Args[0], "-test.run=^TestRetentionMainDrainsInterruptedBootstrap$")
	cmd.Env = []string{
		"HHC_RETENTION_SIGNAL_CHILD=1",
		"ASSET_RETENTION_APPLY_ENABLED=false",
		"DATABASE_URL=postgres://fixture@" + listener.Addr().String() + "/fixture?sslmode=disable",
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := false
	defer func() {
		if !exited {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	var conn net.Conn
	select {
	case conn = <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("worker never reached the controlled loopback bootstrap")
	}
	defer conn.Close()
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		exited = true
		if err != nil {
			t.Fatalf("SIGTERM bypassed cooperative worker exit: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not drain cancelled bootstrap")
	}
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := io.Copy(io.Discard, conn); err != nil {
		t.Fatalf("bootstrap connection was not closed: %v", err)
	}
}

func TestRetentionBootstrapHonorsParentCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	t.Setenv("DATABASE_URL", "postgres://fixture@127.0.0.1:1/fixture?sslmode=disable")
	t.Setenv("ASSET_RETENTION_APPLY_ENABLED", "false")
	if err := run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("bootstrap lost cancellation: %v", err)
	}
}
