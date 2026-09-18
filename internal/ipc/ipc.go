package ipc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"github.com/decfrr/pbotd/internal/model"
)

const Version = 1
const MaxMessage = 8 << 20
const MaxResponse = 128 << 20

type Submission struct {
	Script      string            `json:"script"`
	Filename    string            `json:"filename"`
	CWD         string            `json:"cwd"`
	Environment map[string]string `json:"environment"`
	Args        []string          `json:"args"`
}

type Request struct {
	Version    int         `json:"version"`
	Operation  string      `json:"operation"`
	Submission *Submission `json:"submission,omitempty"`
	IDs        []string    `json:"ids,omitempty"`
	Args       []string    `json:"args,omitempty"`
	All        bool        `json:"all,omitempty"`
	Full       bool        `json:"full,omitempty"`
	AttemptID  string      `json:"attempt_id,omitempty"`
}

type Node struct {
	Hostname  string              `json:"hostname"`
	Capacity  model.Capacity      `json:"capacity"`
	Reserved  model.Resources     `json:"reserved"`
	Inventory model.Inventory     `json:"inventory"`
	Counts    map[model.State]int `json:"job_counts"`
}

type Response struct {
	Version  int             `json:"version"`
	Error    string          `json:"error,omitempty"`
	Code     int             `json:"code,omitempty"`
	JobID    string          `json:"job_id,omitempty"`
	Jobs     []model.Job     `json:"jobs,omitempty"`
	Attempts []model.Attempt `json:"attempts,omitempty"`
	Events   []model.Event   `json:"events,omitempty"`
	Node     *Node           `json:"node,omitempty"`
}

func Failure(err error, code int) Response {
	return Response{Version: Version, Error: err.Error(), Code: code}
}

func readMessage(r io.Reader, target any, limit int64) error {
	reader := bufio.NewReader(io.LimitReader(r, limit+1))
	line, err := reader.ReadBytes('\n')
	if int64(len(line)) > limit {
		return fmt.Errorf("IPC message exceeds %d bytes; narrow the job query if requesting history", limit)
	}
	if err != nil {
		return fmt.Errorf("incomplete IPC message: %w", err)
	}
	d := json.NewDecoder(bytes.NewReader(line))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return fmt.Errorf("invalid IPC JSON: %w", err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("multiple JSON values in one IPC message")
	}
	return nil
}

func Call(ctx context.Context, socket string, request Request) (Response, error) {
	request.Version = Version
	var response Response
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
	if err != nil {
		return response, err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	deadline := time.Now().Add(30 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	conn.SetDeadline(deadline)
	if err := json.NewEncoder(conn).Encode(request); err != nil {
		return response, err
	}
	if err := readMessage(conn, &response, MaxResponse); err != nil {
		return response, err
	}
	if response.Version != Version {
		return response, fmt.Errorf("incompatible daemon protocol %d (client %d)", response.Version, Version)
	}
	return response, nil
}

func Serve(ctx context.Context, listener *net.UnixListener, handler func(context.Context, Request) Response) error {
	stop := context.AfterFunc(ctx, func() { listener.Close() })
	defer stop()
	var wg sync.WaitGroup
	defer wg.Wait()
	connections := make(chan struct{}, 64)
	for {
		conn, err := listener.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		select {
		case connections <- struct{}{}:
		default:
			conn.Close()
			continue
		}
		wg.Go(func() {
			defer func() { <-connections }()
			defer conn.Close()
			stop := context.AfterFunc(ctx, func() { conn.Close() })
			defer stop()
			conn.SetDeadline(time.Now().Add(5 * time.Second))
			var response Response
			uid, err := PeerUID(conn)
			if err != nil || uid != os.Getuid() {
				return
			}
			var request Request
			if err := readMessage(conn, &request, MaxMessage); err != nil {
				response = Failure(err, 2)
			} else if request.Version != Version {
				response = Failure(fmt.Errorf("incompatible client protocol %d (daemon %d)", request.Version, Version), 2)
			} else {
				conn.SetDeadline(time.Now().Add(30 * time.Second))
				response = handler(ctx, request)
			}
			response.Version = Version
			json.NewEncoder(conn).Encode(response)
		})
	}
}
