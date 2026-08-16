package clamav

import (
	"context"
	"errors"
	"os/exec"
	"testing"
	"time"
)

func TestLocalScannerClassifiesClamScanExitCodes(t *testing.T) {
	clean := NewLocalScanner("/signatures", time.Second, 200<<20, 1<<30, 10000, 32)
	var command string
	var args []string
	clean.run = func(_ context.Context, name string, values ...string) ([]byte, error) {
		command, args = name, values
		return []byte("file: OK\n"), nil
	}
	if name, err := clean.ScanFile(context.Background(), "/tmp/file"); err != nil || name != "" {
		t.Fatalf("clean = %q, %v", name, err)
	}
	if command != "clamscan" {
		t.Fatalf("command = %q", command)
	}
	for _, required := range []string{
		"--max-filesize=209715200",
		"--max-scansize=1073741824",
		"--max-files=10000",
		"--max-recursion=32",
		"--alert-exceeds-max=yes",
		"--alert-encrypted=yes",
	} {
		if !contains(args, required) {
			t.Errorf("missing argument %q in %v", required, args)
		}
	}

	infected := NewLocalScanner("/signatures", time.Second, 200<<20, 1<<30, 10000, 32)
	infected.run = func(context.Context, string, ...string) ([]byte, error) {
		return []byte("/tmp/file: Eicar-Signature FOUND\n"), &exec.ExitError{}
	}
	infected.exitCode = func(error) int { return 1 }
	if name, err := infected.ScanFile(context.Background(), "/tmp/file"); !errors.Is(err, ErrInfected) || name != "Eicar-Signature" {
		t.Fatalf("infected = %q, %v", name, err)
	}

	unavailable := NewLocalScanner("/signatures", time.Second, 200<<20, 1<<30, 10000, 32)
	unavailable.run = func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("failed") }
	if _, err := unavailable.ScanFile(context.Background(), "/tmp/file"); err == nil {
		t.Fatal("expected unavailable error")
	}
}

func TestLocalScannerClassifiesLimitsAndEncryptedAsNonMalware(t *testing.T) {
	tests := []struct {
		name, output string
		want         error
	}{
		{name: "scan limit", output: "/tmp/file: Heuristics.Limits.Exceeded.MaxFiles FOUND\n", want: ErrLimitExceeded},
		{name: "encrypted", output: "/tmp/file: Heuristics.Encrypted.PDF FOUND\n", want: ErrEncrypted},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			scanner := NewLocalScanner("/signatures", time.Second, 200<<20, 1<<30, 10000, 32)
			scanner.run = func(context.Context, string, ...string) ([]byte, error) {
				return []byte(test.output), &exec.ExitError{}
			}
			scanner.exitCode = func(error) int { return 1 }
			name, err := scanner.ScanFile(context.Background(), "/tmp/file")
			if !errors.Is(err, test.want) || errors.Is(err, ErrInfected) || name == "" {
				t.Fatalf("name=%q err=%v", name, err)
			}
		})
	}
}

func TestLocalScannerEnforcesTimeout(t *testing.T) {
	scanner := NewLocalScanner("/signatures", 10*time.Millisecond, 200<<20, 1<<30, 10000, 32)
	scanner.run = func(ctx context.Context, _ string, _ ...string) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if _, err := scanner.ScanFile(context.Background(), "/tmp/file"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v", err)
	}
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
