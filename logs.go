package codemode

import (
	"strings"
	"sync"
)

// logCollector gathers console output under three budgets: per-line runes,
// total lines, total bytes. Crossing one caps the collector and tells the
// caller to fail the run — output that silently stops is output the model
// keeps trying to read.
type logCollector struct {
	mu     sync.Mutex
	lines  []string
	bytes  int
	capped bool
	limits Limits
}

func newLogCollector(l Limits) *logCollector { return &logCollector{limits: l} }

func (c *logCollector) append(line string) (over bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.capped {
		return false
	}
	if r := []rune(line); len(r) > c.limits.LogLineRunes {
		line = string(r[:c.limits.LogLineRunes]) + "…(line truncated)"
	}
	c.lines = append(c.lines, line)
	c.bytes += len(line) + 1
	if len(c.lines) >= c.limits.MaxLogLines || c.bytes >= c.limits.LogBudgetBytes {
		c.capped = true
		return true
	}
	return false
}

func (c *logCollector) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.lines))
	copy(out, c.lines)
	return out
}

// TailLogs joins the last lines that fit in budget bytes, keeping the tail
// whole and dropping from the front. Use it to attach output to a failure
// message: the model needs to see what the program printed just before it died,
// and that budget is much tighter than the one a successful run gets.
func TailLogs(lines []string, budget int) string {
	if budget <= 0 || len(lines) == 0 {
		return ""
	}
	total, start := 0, len(lines)
	for i := len(lines) - 1; i >= 0; i-- {
		n := len(lines[i]) + 1
		if total+n > budget {
			break
		}
		total += n
		start = i
	}
	var sb strings.Builder
	if start > 0 {
		sb.WriteString("…(older output omitted)\n")
	}
	for _, l := range lines[start:] {
		sb.WriteString(l)
		sb.WriteByte('\n')
	}
	return strings.TrimRight(sb.String(), "\n")
}
