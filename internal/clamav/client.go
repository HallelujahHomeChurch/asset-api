package clamav

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

const chunkSize = 64 << 10

var ErrInfected = errors.New("malware detected")

type Client struct {
	address     string
	timeout     time.Duration
	maxFileSize int64
	dial        func(context.Context, string, string) (net.Conn, error)
}

func NewClient(host string, port int, timeout time.Duration, maxFileSize int64) *Client {
	dialer := &net.Dialer{Timeout: min(timeout, 10*time.Second), KeepAlive: 30 * time.Second}
	return &Client{
		address: net.JoinHostPort(host, fmt.Sprint(port)), timeout: timeout, maxFileSize: maxFileSize,
		dial: dialer.DialContext,
	}
}

// Scan sends a reader with clamd's INSTREAM protocol. The payload is never
// buffered in full by asset-api.
func (c *Client) Scan(ctx context.Context, source io.Reader, size int64) (string, error) {
	if size <= 0 || size > c.maxFileSize {
		return "", fmt.Errorf("file size %d exceeds ClamAV scan limit %d", size, c.maxFileSize)
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	conn, err := c.dial(ctx, "tcp", c.address)
	if err != nil {
		return "", fmt.Errorf("connect to clamd: %w", err)
	}
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	_ = conn.SetDeadline(deadline)
	if _, err := io.WriteString(conn, "zINSTREAM\x00"); err != nil {
		return "", fmt.Errorf("start clamd stream: %w", err)
	}

	buffer := make([]byte, chunkSize)
	var sent int64
	for {
		read, readErr := source.Read(buffer)
		if read > 0 {
			sent += int64(read)
			if sent > c.maxFileSize {
				return "", fmt.Errorf("stream exceeds ClamAV scan limit %d", c.maxFileSize)
			}
			var length [4]byte
			binary.BigEndian.PutUint32(length[:], uint32(read))
			if _, err := conn.Write(length[:]); err != nil {
				return "", fmt.Errorf("write clamd chunk length: %w", err)
			}
			if _, err := conn.Write(buffer[:read]); err != nil {
				return "", fmt.Errorf("write clamd chunk: %w", err)
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return "", fmt.Errorf("read asset for scan: %w", readErr)
		}
	}
	if sent != size {
		return "", fmt.Errorf("asset size changed while scanning: expected %d, read %d", size, sent)
	}
	if _, err := conn.Write([]byte{0, 0, 0, 0}); err != nil {
		return "", fmt.Errorf("finish clamd stream: %w", err)
	}
	response, err := bufio.NewReader(conn).ReadString(0)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("read clamd response: %w", err)
	}
	response = strings.TrimSpace(strings.TrimSuffix(response, "\x00"))
	switch {
	case strings.HasSuffix(response, " OK"):
		return "", nil
	case strings.HasSuffix(response, " FOUND"):
		name := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(response, "stream:"), "FOUND"))
		return name, fmt.Errorf("%w: %s", ErrInfected, name)
	case strings.HasSuffix(response, " ERROR"):
		return "", fmt.Errorf("clamd rejected stream: %s", response)
	default:
		return "", fmt.Errorf("unexpected clamd response %q", response)
	}
}
