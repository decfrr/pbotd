package ipc

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"
)

func TestMalformedRequestDoesNotBreakServer(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "socket")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, listener, func(ctx context.Context, r Request) Response { return Response{JobID: r.Operation} })
	}()
	for _, body := range []string{"not-json\n", "{\"version\":99}\n", "{\"version\":1,\"surprise\":true}\n", "{} {}\n"} {
		conn, err := net.Dial("unix", socket)
		if err != nil {
			t.Fatal(err)
		}
		conn.SetDeadline(time.Now().Add(time.Second))
		conn.Write([]byte(body))
		var response Response
		err = readMessage(conn, &response, MaxResponse)
		conn.Close()
		if err != nil || response.Error == "" {
			t.Fatalf("%s: %+v %v", body, response, err)
		}
	}
	response, err := Call(ctx, socket, Request{Operation: "alive"})
	if err != nil || response.JobID != "alive" {
		t.Fatalf("%+v %v", response, err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
