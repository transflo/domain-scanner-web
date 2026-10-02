package proxy

import (
	"regexp"
	"strings"
	"time"
)

// A failed probe usually only says "EOF" or "connection reset": the reason lives in xray's own
// error log ("failed to dial to host:443 > lookup host: no such host"). The Manager keeps the
// most recent error lines so a failure can be explained with what xray said about that host.

const maxXrayErrs = 200

type xrayErr struct {
	at   time.Time
	line string
}

var (
	errPrefix = regexp.MustCompile(`^.*?\[Error\]\s*(?:\[\d+\]\s*)?`)
	component = regexp.MustCompile(`^[A-Za-z0-9_./-]+:\s+`)
)

// cleanXrayError drops the timestamp, level, connection id and component from an xray log line.
func cleanXrayError(line string) string {
	s := errPrefix.ReplaceAllString(strings.TrimSpace(line), "")
	s = component.ReplaceAllString(s, "")
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

func (m *Manager) noteError(line string) {
	m.xerrMu.Lock()
	defer m.xerrMu.Unlock()
	m.xerrs = append(m.xerrs, xrayErr{at: time.Now(), line: line})
	if len(m.xerrs) > maxXrayErrs {
		m.xerrs = append(m.xerrs[:0], m.xerrs[len(m.xerrs)-maxXrayErrs:]...)
	}
}

// reasonFor returns the newest error xray logged about host since the given time, or "".
func (m *Manager) reasonFor(host string, since time.Time) string {
	if host == "" {
		return ""
	}
	m.xerrMu.Lock()
	defer m.xerrMu.Unlock()
	for i := len(m.xerrs) - 1; i >= 0; i-- {
		e := m.xerrs[i]
		if e.at.Before(since) {
			break
		}
		if strings.Contains(e.line, host) {
			return cleanXrayError(e.line)
		}
	}
	return ""
}

// explain adds xray's reason to a failed probe, keeping the probe's own error in brackets.
// xray writes its error line about as the connection fails, so look a little before the probe
// started (job traffic through the same proxy may already have logged it) and wait briefly for
// the line to arrive through the log pipe.
func (m *Manager) explain(r ProbeResult, host string, since time.Time) ProbeResult {
	if r.OK {
		return r
	}
	since = since.Add(-explainLookback)
	for i := 0; ; i++ {
		if why := m.reasonFor(host, since); why != "" {
			r.Error = why + "(探测:" + r.Error + ")"
			return r
		}
		if i >= explainPolls {
			return r
		}
		time.Sleep(explainPoll)
	}
}

const (
	explainLookback = 2 * time.Second
	explainPoll     = 40 * time.Millisecond
	explainPolls    = 5
)
