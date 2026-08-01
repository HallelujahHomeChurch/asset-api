package clamav

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

type LocalScanner struct {
	database string
	timeout  time.Duration
	run      func(context.Context, string, ...string) ([]byte, error)
	exitCode func(error) int
}

func NewLocalScanner(database string, timeout time.Duration) *LocalScanner {
	return &LocalScanner{
		database: database,
		timeout:  timeout,
		run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, name, args...).CombinedOutput()
		},
		exitCode: func(err error) int {
			if value, ok := err.(*exec.ExitError); ok {
				return value.ExitCode()
			}
			return -1
		},
	}
}

func (s *LocalScanner) ScanFile(ctx context.Context, filePath string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	output, err := s.run(ctx, "clamscan", "--no-summary", "--stdout", "--database="+s.database, "--", filePath)
	if err == nil {
		return "", nil
	}
	if s.exitCode(err) == 1 {
		line := strings.TrimSpace(string(output))
		line = strings.TrimSuffix(line, " FOUND")
		if colon := strings.LastIndex(line, ": "); colon >= 0 {
			line = line[colon+2:]
		}
		return line, fmt.Errorf("%w: %s", ErrInfected, line)
	}
	return "", fmt.Errorf("clamscan unavailable: %w", err)
}
