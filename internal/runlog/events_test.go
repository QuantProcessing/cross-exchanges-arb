package runlog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestEventSinkRecordWritesJSONL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("Create(%s): %v", path, err)
	}

	sink := NewEventSink(file)
	if err := sink.Record(Event{
		Category: "session",
		Type:     "started",
		Symbol:   "BTC",
	}); err != nil {
		t.Fatalf("Record() error = %v", err)
	}
	if err := sink.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}

	var got Event
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal() error = %v; data=%s", err, data)
	}
	if got.Category != "session" {
		t.Fatalf("Category = %q, want session", got.Category)
	}
	if got.Type != "started" {
		t.Fatalf("Type = %q, want started", got.Type)
	}
	if got.Symbol != "BTC" {
		t.Fatalf("Symbol = %q, want BTC", got.Symbol)
	}
	if got.At.IsZero() {
		t.Fatal("At should be set")
	}
	if got.At.Location() != time.UTC {
		t.Fatalf("At location = %v, want UTC", got.At.Location())
	}
}
