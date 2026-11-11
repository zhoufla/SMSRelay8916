package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/zhoufla/SMSRelay8916/internal/config"
	"github.com/zhoufla/SMSRelay8916/internal/localrpc"
	"github.com/zhoufla/SMSRelay8916/internal/logbuf"
	"github.com/zhoufla/SMSRelay8916/internal/modem"
	"github.com/zhoufla/SMSRelay8916/internal/store"
)

func TestRPCValidationAndIdempotentSend(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "sms.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a := &App{store: s, cfg: &config.Config{}, logs: logbuf.New(5)}
	for _, tc := range []struct{ method, params string }{
		{"exec", `{}`}, {"send", `{"request_id":"1234567890123456","number":"--help","text":"test"}`},
		{"send", `{"request_id":"short","number":"10086","text":"test"}`}, {"delete", `{"ids":[]}`},
		{"messages", `{"dir":"bad"}`}, {"test_channel", `{"section":"missing"}`},
	} {
		if _, err := a.dispatch(tc.method, json.RawMessage(tc.params)); err == nil {
			t.Fatalf("accepted invalid %s: %s", tc.method, tc.params)
		}
	}
	p := json.RawMessage(`{"request_id":"1234567890123456","number":"10086","text":"a,b\n中"}`)
	for i := 0; i < 2; i++ {
		if _, err := a.dispatch("send", p); err != nil {
			t.Fatal(err)
		}
	}
	items, total, err := s.List(store.ListQuery{Dir: "out", Limit: 30})
	if err != nil || total != 1 || items[0].Body != "a,b\n中" {
		t.Fatalf("queue: %+v %d %v", items, total, err)
	}
}

func TestLocalRPCRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "sms.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a := &App{store: s, cfg: &config.Config{}, logs: logbuf.New(5)}
	path := filepath.Join(dir, "control.sock")
	l, err := localrpc.Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- localrpc.Serve(ctx, l, a.dispatch) }()
	for _, req := range []localrpc.Request{
		{Method: "send", Params: json.RawMessage(`{"request_id":"roundtrip-12345678","number":"10086","text":"quote,\n中文"}`)},
		{Method: "messages", Params: json.RawMessage(`{"dir":"out","limit":30}`)},
		{Method: "logs", Params: json.RawMessage(`{"limit":500}`)},
	} {
		out, err := localrpc.Call(path, req)
		if err != nil {
			t.Fatal(err)
		}
		var resp struct {
			OK bool `json:"ok"`
		}
		if err := json.Unmarshal(out, &resp); err != nil || !resp.OK {
			t.Fatalf("%s: %s %v", req.Method, out, err)
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestUnsentModemObjectIsNotDeleted(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "deleted")
	script := "#!/bin/sh\ncase \"$*\" in\n*--messaging-delete-sms*) touch '" + marker + "';;\n*) echo '{\"sms\":{\"content\":{\"number\":\"10086\",\"text\":\"draft\"},\"properties\":{\"pdu-type\":\"submit\",\"state\":\"draft\"}}}';;\nesac\n"
	if err := os.WriteFile(filepath.Join(dir, "mmcli"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	s, err := store.Open(filepath.Join(dir, "sms.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a := &App{store: s, modem: modem.New("0"), states: map[string]*smsState{}}
	a.process("/org/freedesktop/ModemManager1/SMS/1")
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("unsent modem object was deleted")
	}
	_, total, err := s.List(store.ListQuery{Dir: "out", Limit: 30})
	if err != nil || total != 0 {
		t.Fatal("unsent object must not be archived as sent")
	}
}

func TestSplitBodyLabels(t *testing.T) {
	parts := splitBody(strings.Repeat("中", 1000))
	for i, p := range parts {
		if len([]rune(p)) > 70 {
			t.Fatalf("part %d too long", i)
		}
		if !strings.HasSuffix(p, fmt.Sprintf("(%d/%d)", i+1, len(parts))) {
			t.Fatalf("part %d missing label", i)
		}
	}
}

func TestSplitEmojiUsesUTF16Units(t *testing.T) {
	parts := splitBody(strings.Repeat("😀", 60))
	if len(parts) != 2 {
		t.Fatalf("emoji parts=%d", len(parts))
	}
	for _, p := range parts {
		if len(utf16.Encode([]rune(p))) > 70 {
			t.Fatal("emoji segment exceeds UCS2 budget")
		}
	}
}

func TestPersistentDatabasePaths(t *testing.T) {
	for _, p := range []string{"/tmp/a.db", "/var/a.db", "relative.db", "/etc/../tmp/a.db", "/etc/smsrelay/upgrade.db"} {
		if validDBPath(p) {
			t.Fatalf("accepted %s", p)
		}
	}
	if !validDBPath("/etc/smsrelay/sms.db") {
		t.Fatal("default path rejected")
	}
}
