package clamav

import (
	"context"
	"errors"
	"os/exec"
	"testing"
	"time"
)

func TestLocalScannerClassifiesClamScanExitCodes(t *testing.T) {
	clean := NewLocalScanner("/signatures", time.Second)
	clean.run = func(context.Context, string, ...string) ([]byte, error) { return []byte("file: OK\n"), nil }
	if name, err := clean.ScanFile(context.Background(), "/tmp/file"); err != nil || name != "" {
		t.Fatalf("clean = %q, %v", name, err)
	}

	infected := NewLocalScanner("/signatures", time.Second)
	infected.run = func(context.Context, string, ...string) ([]byte, error) {
		return []byte("/tmp/file: Eicar-Signature FOUND\n"), &exec.ExitError{}
	}
	infected.exitCode = func(error) int { return 1 }
	if name, err := infected.ScanFile(context.Background(), "/tmp/file"); !errors.Is(err, ErrInfected) || name != "Eicar-Signature" {
		t.Fatalf("infected = %q, %v", name, err)
	}

	unavailable := NewLocalScanner("/signatures", time.Second)
	unavailable.run = func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("failed") }
	if _, err := unavailable.ScanFile(context.Background(), "/tmp/file"); err == nil {
		t.Fatal("expected unavailable error")
	}
}
