package clamav

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

var (
	ErrLimitExceeded = errors.New("ClamAV scan limit exceeded")
	ErrEncrypted     = errors.New("encrypted content rejected")
)

type LocalScanner struct {
	database     string
	timeout      time.Duration
	maxFileSize  int64
	maxScanSize  int64
	maxFiles     int
	maxRecursion int
	run          func(context.Context, string, ...string) ([]byte, error)
	exitCode     func(error) int
}

func NewLocalScanner(database string, timeout time.Duration, maxFileSize, maxScanSize int64, maxFiles, maxRecursion int) *LocalScanner {
	return &LocalScanner{
		database: database, timeout: timeout, maxFileSize: maxFileSize, maxScanSize: maxScanSize,
		maxFiles: maxFiles, maxRecursion: maxRecursion,
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
	output, err := s.run(ctx, "clamscan",
		"--no-summary", "--stdout", "--database="+s.database,
		fmt.Sprintf("--max-filesize=%d", s.maxFileSize),
		fmt.Sprintf("--max-scansize=%d", s.maxScanSize),
		fmt.Sprintf("--max-files=%d", s.maxFiles),
		fmt.Sprintf("--max-recursion=%d", s.maxRecursion),
		"--alert-exceeds-max=yes",
		"--alert-encrypted=yes",
		"--", filePath,
	)
	if err == nil {
		return "", nil
	}
	if s.exitCode(err) == 1 {
		line := strings.TrimSpace(string(output))
		line = strings.TrimSuffix(line, " FOUND")
		if colon := strings.LastIndex(line, ": "); colon >= 0 {
			line = line[colon+2:]
		}
		switch {
		case strings.HasPrefix(line, "Heuristics.Limits.Exceeded"):
			return line, fmt.Errorf("%w: %s", ErrLimitExceeded, line)
		case strings.HasPrefix(line, "Heuristics.Encrypted"):
			return line, fmt.Errorf("%w: %s", ErrEncrypted, line)
		}
		return line, fmt.Errorf("%w: %s", ErrInfected, line)
	}
	return "", fmt.Errorf("clamscan unavailable: %w", err)
}
