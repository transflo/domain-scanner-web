// Package enumerate produces domain candidates by index so a scan can be resumed from a cursor.
// Unlike upstream's channel-based generator it never prints or exits, and At(i) is a pure
// function of the Spec, which is what makes resume-after-restart safe.
package enumerate

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/dlclark/regexp2"
)

// ErrTooLarge is returned when the candidate space exceeds the allowed maximum (or overflows).
var ErrTooLarge = errors.New("search space too large")

const (
	maxLabel      = 63
	maxRegexLen   = 200
	maxQuantifier = 5
	regexTimeout  = 100 * time.Millisecond
	letters       = "abcdefghijklmnopqrstuvwxyz"
	digits        = "0123456789"
)

// Spec describes what to enumerate. Words != nil selects dictionary mode (Length and Pattern
// are then ignored).
type Spec struct {
	Length  int
	Suffix  string
	Pattern string // d = digits, D = letters, a = alphanumeric
	Regex   string // applied to the label (without suffix)
	Words   []string
}

// Plan is a compiled Spec.
type Plan struct {
	suffix  string
	charset string
	length  int
	words   []string
	dict    bool
	total   int64

	mu    sync.Mutex
	regex *regexp2.Regexp
}

// Compile validates the spec. maxSpace <= 0 means "no limit" (overflow is still rejected).
func Compile(s Spec, maxSpace int64) (*Plan, error) {
	suffix, err := normaliseSuffix(s.Suffix)
	if err != nil {
		return nil, err
	}
	p := &Plan{suffix: suffix}

	if s.Regex != "" {
		if err := validateRegex(s.Regex); err != nil {
			return nil, err
		}
		re, err := regexp2.Compile(s.Regex, regexp2.None)
		if err != nil {
			return nil, fmt.Errorf("invalid regex: %w", err)
		}
		re.MatchTimeout = regexTimeout
		p.regex = re
	}

	if s.Words != nil {
		if len(s.Words) == 0 {
			return nil, errors.New("dictionary is empty")
		}
		p.dict = true
		p.words = s.Words
		p.total = int64(len(s.Words))
		return p, nil
	}

	switch s.Pattern {
	case "d":
		p.charset = digits
	case "D":
		p.charset = letters
	case "a":
		p.charset = letters + digits
	default:
		return nil, fmt.Errorf("invalid pattern %q (use d, D or a)", s.Pattern)
	}
	if s.Length <= 0 {
		return nil, errors.New("length must be at least 1")
	}
	if s.Length > maxLabel {
		return nil, fmt.Errorf("length must be at most %d", maxLabel)
	}
	p.length = s.Length

	total := int64(1)
	size := int64(len(p.charset))
	for i := 0; i < s.Length; i++ {
		if total > math.MaxInt64/size {
			return nil, fmt.Errorf("%w: %d^%d overflows", ErrTooLarge, size, s.Length)
		}
		total *= size
	}
	if maxSpace > 0 && total > maxSpace {
		return nil, fmt.Errorf("%w: %d candidates exceeds limit %d", ErrTooLarge, total, maxSpace)
	}
	p.total = total
	return p, nil
}

// Total is the unfiltered number of candidates; cursors index into [0, Total).
func (p *Plan) Total() int64 { return p.total }

// At returns candidate i. ok=false means it was filtered out (regex, invalid label) or i is out
// of range; callers must still advance the cursor past it.
func (p *Plan) At(i int64) (string, bool) {
	if i < 0 || i >= p.total {
		return "", false
	}
	var label string
	if p.dict {
		label = strings.ToLower(strings.TrimSpace(p.words[i]))
		if !validLabel(label) {
			return "", false
		}
	} else {
		size := int64(len(p.charset))
		buf := make([]byte, p.length)
		for pos := p.length - 1; pos >= 0; pos-- {
			buf[pos] = p.charset[i%size]
			i /= size
		}
		label = string(buf)
	}
	if p.regex != nil {
		p.mu.Lock()
		match, err := p.regex.MatchString(label)
		p.mu.Unlock()
		if err != nil || !match {
			return "", false
		}
	}
	return label + p.suffix, true
}

func normaliseSuffix(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return "", errors.New("suffix is required")
	}
	if !strings.HasPrefix(s, ".") {
		s = "." + s
	}
	for _, part := range strings.Split(s[1:], ".") {
		if !validLabel(part) {
			return "", fmt.Errorf("invalid suffix %q", s)
		}
	}
	return s, nil
}

// validLabel reports whether s is a valid ASCII DNS label.
func validLabel(s string) bool {
	if s == "" || len(s) > maxLabel || s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

// validateRegex mirrors upstream's ReDoS guard (length, known-bad patterns, quantifier count).
func validateRegex(pattern string) error {
	if len(pattern) > maxRegexLen {
		return fmt.Errorf("regex too long (max %d characters)", maxRegexLen)
	}
	for _, bad := range []string{"(.*)*", "(.+)+", "(a+)+", "(a*)*", "(.{0,})*", `(\w+)*\w*`} {
		if strings.Contains(pattern, bad) {
			return fmt.Errorf("regex contains potentially dangerous pattern %s", bad)
		}
	}
	if strings.Count(pattern, "+")+strings.Count(pattern, "*") > maxQuantifier {
		return fmt.Errorf("too many quantifiers in regex (max %d)", maxQuantifier)
	}
	return nil
}
