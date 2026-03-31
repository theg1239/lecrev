package timetrace

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

type Recorder struct {
	mu    sync.Mutex
	lines []string
}

func New() *Recorder {
	return &Recorder{}
}

func (r *Recorder) Step(step string, started time.Time, details ...string) {
	r.Add(step, time.Since(started), details...)
}

func (r *Recorder) Add(step string, elapsed time.Duration, details ...string) {
	if r == nil {
		return
	}
	line := fmt.Sprintf("[platform] step=%s durationMs=%d", sanitize(step), maxInt64(0, elapsed.Milliseconds()))
	for _, detail := range details {
		detail = strings.TrimSpace(detail)
		if detail == "" {
			continue
		}
		line += " " + detail
	}
	r.mu.Lock()
	r.lines = append(r.lines, line)
	r.mu.Unlock()
}

func (r *Recorder) Note(step string, details ...string) {
	r.Add(step, 0, details...)
}

func (r *Recorder) String() string {
	if r == nil {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.TrimSpace(strings.Join(r.lines, "\n"))
}

func Combine(trace, logs string) string {
	trace = strings.TrimSpace(trace)
	logs = strings.TrimSpace(logs)
	switch {
	case trace == "":
		return logs
	case logs == "":
		return trace
	default:
		return trace + "\n" + logs
	}
}

func sanitize(step string) string {
	step = strings.TrimSpace(step)
	if step == "" {
		return "unknown"
	}
	step = strings.ReplaceAll(step, " ", "_")
	return step
}

func maxInt64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}
