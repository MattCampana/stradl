package agent

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// MaxLogSize is the size at which the agent log is rotated. One old file
// (agent.log.1) is kept.
const MaxLogSize = 5 << 20

// LogFile is an append-only log that rotates itself.
//
// Under launchd the same file is also the process's stdout and stderr, so a
// crash trace lands next to the agent's own lines. On rotation stdout and
// stderr are moved to the fresh file too when they pointed at the old one.
type LogFile struct {
	mu   sync.Mutex
	path string
	max  int64
	f    *os.File
}

// OpenLogFile opens (creating if needed) the log at path.
func OpenLogFile(path string, max int64) (*LogFile, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return nil, err
	}
	return &LogFile{path: path, max: max, f: f}, nil
}

// Write appends p, rotating first when the file would grow past the limit.
// The size is read from the file each time because launchd appends to it
// too.
func (l *LogFile) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if fi, err := l.f.Stat(); err == nil && fi.Size() > 0 && fi.Size()+int64(len(p)) > l.max {
		l.rotate(fi)
	}
	return l.f.Write(p)
}

func (l *LogFile) rotate(old os.FileInfo) {
	if err := os.Rename(l.path, l.path+".1"); err != nil {
		return // keep writing to the current file rather than losing lines
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return
	}
	_ = l.f.Close()
	l.f = f
	redirectStdio(f, old)
}

// Close closes the file.
func (l *LogFile) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Close()
}

// Logger writes timestamped lines.
type Logger struct {
	w   io.Writer
	now func() time.Time
}

// NewLogger returns a Logger writing to w; a nil w discards everything.
func NewLogger(w io.Writer) *Logger {
	if w == nil {
		w = io.Discard
	}
	return &Logger{w: w, now: time.Now}
}

// Printf writes one line.
func (l *Logger) Printf(format string, args ...any) {
	if l == nil {
		return
	}
	line := l.now().Format("2006-01-02 15:04:05") + " " + fmt.Sprintf(format, args...)
	if line[len(line)-1] != '\n' {
		line += "\n"
	}
	_, _ = io.WriteString(l.w, line)
}
