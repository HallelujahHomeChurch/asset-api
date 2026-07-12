package clamav

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

func TestScanStreamsCleanFile(t *testing.T) {
	client, received := testClient(t, "stream: OK\x00")
	payload := bytes.Repeat([]byte("a"), chunkSize+17)
	name, err := client.Scan(context.Background(), bytes.NewReader(payload), int64(len(payload)))
	if err != nil || name != "" {
		t.Fatalf("Scan() = %q, %v", name, err)
	}
	if !bytes.Equal(<-received, payload) {
		t.Fatal("clamd did not receive the complete payload")
	}
}

func TestScanReportsMalwareName(t *testing.T) {
	client, _ := testClient(t, "stream: Eicar-Signature FOUND\x00")
	name, err := client.Scan(context.Background(), bytes.NewReader([]byte("eicar")), 5)
	if !errors.Is(err, ErrInfected) || name != "Eicar-Signature" {
		t.Fatalf("Scan() = %q, %v", name, err)
	}
}

func TestScanRejectsOversizedFileBeforeConnecting(t *testing.T) {
	client := NewClient("unused", 3310, time.Second, 4)
	_, err := client.Scan(context.Background(), bytes.NewReader([]byte("large")), 5)
	if err == nil {
		t.Fatal("expected size limit error")
	}
}

func testClient(t *testing.T, response string) (*Client, <-chan []byte) {
	t.Helper()
	server, clientConn := net.Pipe()
	received := make(chan []byte, 1)
	client := NewClient("test", 3310, time.Second, 2<<20)
	client.dial = func(context.Context, string, string) (net.Conn, error) { return clientConn, nil }
	go func() {
		defer server.Close()
		command := make([]byte, len("zINSTREAM\x00"))
		_, _ = io.ReadFull(server, command)
		var payload bytes.Buffer
		for {
			var length [4]byte
			_, _ = io.ReadFull(server, length[:])
			size := binary.BigEndian.Uint32(length[:])
			if size == 0 {
				break
			}
			_, _ = io.CopyN(&payload, server, int64(size))
		}
		received <- payload.Bytes()
		_, _ = io.WriteString(server, response)
	}()
	return client, received
}
