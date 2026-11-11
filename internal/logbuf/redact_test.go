package logbuf

import (
	"bytes"
	"strings"
	"testing"
)

func TestRedactingWriter(t *testing.T) {
	var out bytes.Buffer
	w := RedactingWriter(&out, []string{"example-secret", ""})
	input := "WARN example-secret https://example.com/hook?access_token=private 13800000000"
	if n, err := w.Write([]byte(input)); err != nil || n != len(input) {
		t.Fatalf("write %d %v", n, err)
	}
	for _, secret := range []string{"example-secret", "access_token", "13800000000"} {
		if strings.Contains(out.String(), secret) {
			t.Fatalf("leaked %q", secret)
		}
	}
}

func TestErrorLogLevel(t *testing.T) {
	b := New(5)
	_, _ = b.Write([]byte("ERROR database unavailable"))
	if entries := b.Snapshot(5, "error"); len(entries) != 1 || entries[0].Level != "error" {
		t.Fatalf("entries: %+v", entries)
	}
}
