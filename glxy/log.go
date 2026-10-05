package glxy

import (
	"fmt"
	"sync"
	"time"

	toolv1 "glxymesh.com/sdk/internal/toolv1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Log lines ride back with the call's result, so tool Lambdas write nothing
// to CloudWatch; past 64 KB a call's lines are dropped and the result says so.
const maxLogBytes = 64 << 10

type Logger struct {
	mu        sync.Mutex
	lines     []*toolv1.LogLine
	size      int
	truncated bool
}

func (l *Logger) Info(format string, args ...any)  { l.add("info", format, args) }
func (l *Logger) Warn(format string, args ...any)  { l.add("warn", format, args) }
func (l *Logger) Error(format string, args ...any) { l.add("error", format, args) }

func (l *Logger) add(level, format string, args []any) {
	msg := fmt.Sprintf(format, args...)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.size+len(msg) > maxLogBytes {
		l.truncated = true
		return
	}
	l.size += len(msg)
	l.lines = append(l.lines, &toolv1.LogLine{At: timestamppb.New(time.Now()), Level: level, Message: msg})
}

func (l *Logger) drain() ([]*toolv1.LogLine, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lines, l.truncated
}
