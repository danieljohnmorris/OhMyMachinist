package runner

import (
	"bytes"
	"fmt"
	"os"
	"sync"
)

// RawLogFileName is the raw agent output artifact: every byte of agent stdout
// with the same secret redaction as the transcript, capped at a configurable
// size so a runaway run cannot fill the disk.
const RawLogFileName = "agent-output.log"

// DefaultRawLogMaxBytes caps the raw agent log when no limit is configured.
const DefaultRawLogMaxBytes = 64 << 20

// rawLog writes the complete agent stdout to a file in the run directory. Lines
// are redacted as they complete so a secret split across read chunks is still
// masked; once the byte cap is reached it appends a marker and stops.
type rawLog struct {
	mu        sync.Mutex
	file      *os.File
	path      string
	limit     int64
	written   int64
	partial   []byte
	truncated bool
}

func newRawLog(path string, limit int64) (*rawLog, error) {
	if limit <= 0 {
		limit = DefaultRawLogMaxBytes
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create raw agent log: %w", err)
	}
	return &rawLog{file: file, path: path, limit: limit}, nil
}

// Write records stdout as it streams. It is best effort: a write error leaves
// the file short but never fails the run, since the typed transcript still
// carries the run's outcome.
func (log *rawLog) Write(data []byte) {
	log.mu.Lock()
	defer log.mu.Unlock()
	if log.truncated || log.file == nil {
		return
	}
	log.partial = append(log.partial, data...)
	for {
		newline := bytes.IndexByte(log.partial, '\n')
		if newline < 0 {
			break
		}
		log.writeLine(log.partial[:newline+1])
		log.partial = log.partial[newline+1:]
	}
	// A single line longer than the cap would otherwise sit in memory until the
	// stream ends; flush (and truncate) it once it reaches the limit.
	if !log.truncated && int64(len(log.partial)) >= log.limit {
		log.writeLine(log.partial)
		log.partial = nil
	}
}

func (log *rawLog) writeLine(line []byte) {
	if log.truncated {
		return
	}
	redacted := redactSecrets(string(line))
	remaining := log.limit - log.written
	if int64(len(redacted)) > remaining {
		if remaining > 0 {
			_, _ = log.file.WriteString(redacted[:remaining])
		}
		log.written = log.limit
		log.truncated = true
		_, _ = log.file.WriteString(fmt.Sprintf("[raw agent log truncated at %d MB]\n", log.limit>>20))
		return
	}
	if _, err := log.file.WriteString(redacted); err != nil {
		log.truncated = true
		return
	}
	log.written += int64(len(redacted))
}

// close flushes the trailing partial line and closes the file.
func (log *rawLog) close() error {
	log.mu.Lock()
	defer log.mu.Unlock()
	if log.file == nil {
		return nil
	}
	if len(log.partial) > 0 {
		log.writeLine(log.partial)
		log.partial = nil
	}
	if err := log.file.Sync(); err != nil {
		_ = log.file.Close()
		log.file = nil
		return err
	}
	err := log.file.Close()
	log.file = nil
	return err
}
