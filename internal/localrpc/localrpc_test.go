package localrpc

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestUnixRPC(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rpc.sock")
	l, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, l, func(method string, p json.RawMessage) (interface{}, error) {
			return map[string]string{"method": method}, nil
		})
	}()
	out, err := Call(path, Request{Method: "status", Params: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		OK   bool              `json:"ok"`
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(out, &response); err != nil || !response.OK || response.Data["method"] != "status" {
		t.Fatalf("response %s: %v", out, err)
	}
	if _, err := Listen(path); err == nil {
		t.Fatal("must not replace live socket")
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatal("socket must be private")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestDoNotReplaceRegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rpc.sock")
	if err := os.WriteFile(path, []byte("user data"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(path); err == nil {
		t.Fatal("must preserve unexpected file")
	}
}
