package runlog

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type SessionOptions struct {
	MakerExchange string
	TakerExchange string
	Symbol        string
	StartedAt     time.Time
}

type Session struct {
	Dir        string
	RunLogPath string
	RawPath    string
	EventsPath string
	RunLogFile *os.File
	RawFile    *os.File
	Events     *EventSink
}

func NewSession(root string, opts SessionOptions) (*Session, error) {
	if root == "" {
		root = "logs"
	}
	startedAt := opts.StartedAt.UTC()
	if startedAt.IsZero() {
		startedAt = time.Now().UTC()
	}

	dir := filepath.Join(root, fmt.Sprintf("%s_%s_%s_%s",
		startedAt.Format("20060102_150405"),
		opts.MakerExchange,
		opts.TakerExchange,
		opts.Symbol,
	))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create run log dir: %w", err)
	}

	session := &Session{
		Dir:        dir,
		RunLogPath: filepath.Join(dir, "run.log"),
		RawPath:    filepath.Join(dir, "raw.jsonl"),
		EventsPath: filepath.Join(dir, "events.jsonl"),
	}

	var err error
	session.RunLogFile, err = os.Create(session.RunLogPath)
	if err != nil {
		return nil, fmt.Errorf("create run.log: %w", err)
	}
	session.RawFile, err = os.Create(session.RawPath)
	if err != nil {
		_ = session.RunLogFile.Close()
		return nil, fmt.Errorf("create raw.jsonl: %w", err)
	}
	eventsFile, err := os.Create(session.EventsPath)
	if err != nil {
		_ = session.RawFile.Close()
		_ = session.RunLogFile.Close()
		return nil, fmt.Errorf("create events.jsonl: %w", err)
	}
	session.Events = NewEventSink(eventsFile)

	return session, nil
}

func (s *Session) Close() error {
	if s == nil {
		return nil
	}

	var firstErr error
	if s.Events != nil {
		if err := s.Events.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if s.RunLogFile != nil {
		if err := s.RunLogFile.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if s.RawFile != nil {
		if err := s.RawFile.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
