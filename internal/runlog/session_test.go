package runlog

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNewSessionCreatesRunDirectoryAndFiles(t *testing.T) {
	root := t.TempDir()
	startedAt := time.Date(2026, 4, 3, 12, 34, 56, 0, time.UTC)

	session, err := NewSession(root, SessionOptions{
		MakerExchange: "EDGEX",
		TakerExchange: "LIGHTER",
		Symbol:        "BTC",
		StartedAt:     startedAt,
	})
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	defer session.Close()

	wantDir := filepath.Join(root, "20260403_123456_EDGEX_LIGHTER_BTC")
	if session.Dir != wantDir {
		t.Fatalf("session.Dir = %q, want %q", session.Dir, wantDir)
	}

	for _, path := range []string{session.RunLogPath, session.RawPath, session.EventsPath} {
		info, statErr := os.Stat(path)
		if statErr != nil {
			t.Fatalf("Stat(%s) error = %v", path, statErr)
		}
		if info.IsDir() {
			t.Fatalf("%s should be a file, got directory", path)
		}
	}
}
