package ipc

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestLargeHistoryResponseKeepsSmallerRequestLimit(t *testing.T) {
	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(Response{Version: Version, Error: strings.Repeat("x", MaxMessage)}); err != nil {
		t.Fatal(err)
	}
	var response Response
	if err := readMessage(bytes.NewReader(body.Bytes()), &response, MaxResponse); err != nil {
		t.Fatal(err)
	}
	if err := readMessage(bytes.NewReader(body.Bytes()), &response, MaxMessage); err == nil {
		t.Fatal("request limit bypassed")
	}
}
