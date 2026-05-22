package slack

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCursorRoundtrip(t *testing.T) {
	dir := t.TempDir()
	store, err := NewCursorStore(filepath.Join(dir, "cursors"))
	if err != nil {
		t.Fatalf("NewCursorStore: %v", err)
	}

	if got, err := store.GetChannelCursor("C1"); err != nil {
		t.Fatalf("GetChannelCursor: %v", err)
	} else if got != "" {
		t.Errorf("fresh cursor should be empty, got %q", got)
	}

	if err := store.SetChannelCursor("C1", "1715814999.000400"); err != nil {
		t.Fatalf("SetChannelCursor: %v", err)
	}
	got, err := store.GetChannelCursor("C1")
	if err != nil {
		t.Fatalf("GetChannelCursor: %v", err)
	}
	if got != "1715814999.000400" {
		t.Errorf("got %q", got)
	}

	if err := store.SetThreadCursor("slack-C1-1715814123.001200", "1715814555.001500"); err != nil {
		t.Fatalf("SetThreadCursor: %v", err)
	}
	tgot, err := store.GetThreadCursor("slack-C1-1715814123.001200")
	if err != nil {
		t.Fatalf("GetThreadCursor: %v", err)
	}
	if tgot != "1715814555.001500" {
		t.Errorf("thread cursor got %q", tgot)
	}
}

func TestCursorAtomicNoTmpLeftover(t *testing.T) {
	dir := t.TempDir()
	store, err := NewCursorStore(filepath.Join(dir, "cursors"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetChannelCursor("C1", "123.000"); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "cursors", "channels"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp") {
			t.Errorf("temp file leftover: %s", e.Name())
		}
	}
}
