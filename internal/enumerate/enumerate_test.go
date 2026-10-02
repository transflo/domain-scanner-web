package enumerate

import (
	"errors"
	"strings"
	"testing"
)

const noLimit = int64(1) << 62

func mustCompile(t *testing.T, s Spec) *Plan {
	t.Helper()
	p, err := Compile(s, noLimit)
	if err != nil {
		t.Fatalf("Compile(%+v): %v", s, err)
	}
	return p
}

func TestDigitsLengthTwo(t *testing.T) {
	p := mustCompile(t, Spec{Length: 2, Suffix: ".li", Pattern: "d"})
	if p.Total() != 100 {
		t.Fatalf("Total = %d, want 100", p.Total())
	}
	if d, ok := p.At(0); !ok || d != "00.li" {
		t.Fatalf("At(0) = %q,%v", d, ok)
	}
	if d, ok := p.At(99); !ok || d != "99.li" {
		t.Fatalf("At(99) = %q,%v", d, ok)
	}
}

func TestPatternSizes(t *testing.T) {
	if got := mustCompile(t, Spec{Length: 1, Suffix: ".com", Pattern: "D"}).Total(); got != 26 {
		t.Fatalf("letters len1 = %d, want 26", got)
	}
	if got := mustCompile(t, Spec{Length: 2, Suffix: ".com", Pattern: "a"}).Total(); got != 1296 {
		t.Fatalf("alnum len2 = %d, want 1296", got)
	}
}

func TestSuffixDotIsAddedWhenMissing(t *testing.T) {
	p := mustCompile(t, Spec{Length: 1, Suffix: "li", Pattern: "d"})
	if d, _ := p.At(3); d != "3.li" {
		t.Fatalf("At(3) = %q, want 3.li", d)
	}
}

func TestDeterministic(t *testing.T) {
	a := mustCompile(t, Spec{Length: 3, Suffix: ".li", Pattern: "a"})
	b := mustCompile(t, Spec{Length: 3, Suffix: ".li", Pattern: "a"})
	for i := int64(0); i < 500; i++ {
		x, _ := a.At(i)
		y, _ := b.At(i)
		if x != y {
			t.Fatalf("At(%d) differs: %q vs %q", i, x, y)
		}
	}
}

func TestRegexFiltersButKeepsIndexing(t *testing.T) {
	p := mustCompile(t, Spec{Length: 2, Suffix: ".li", Pattern: "d", Regex: "^1"})
	if d, ok := p.At(10); !ok || d != "10.li" {
		t.Fatalf("At(10) = %q,%v want 10.li,true", d, ok)
	}
	if _, ok := p.At(20); ok {
		t.Fatalf("At(20) should be filtered out")
	}
	if p.Total() != 100 {
		t.Fatalf("Total must stay the unfiltered space, got %d", p.Total())
	}
}

func TestRejectsBadRegexWithoutExiting(t *testing.T) {
	cases := map[string]string{
		"nested quantifier": "(a+)+",
		"too long":          strings.Repeat("a", 201),
		"too many quant":    "a+b+c+d+e+f+",
		"invalid syntax":    "([",
	}
	for name, re := range cases {
		if _, err := Compile(Spec{Length: 2, Suffix: ".li", Pattern: "D", Regex: re}, noLimit); err == nil {
			t.Errorf("%s: expected error for %q", name, re)
		}
	}
}

func TestRejectsInvalidSpecs(t *testing.T) {
	bad := []Spec{
		{Length: 2, Suffix: ".li", Pattern: "x"},
		{Length: 0, Suffix: ".li", Pattern: "d"},
		{Length: -1, Suffix: ".li", Pattern: "d"},
		{Length: 2, Suffix: "", Pattern: "d"},
		{Length: 2, Suffix: ".l i", Pattern: "d"},
		{Length: 2, Suffix: ".", Pattern: "d"},
	}
	for _, s := range bad {
		if _, err := Compile(s, noLimit); err == nil {
			t.Errorf("expected error for %+v", s)
		}
	}
}

func TestTooLargeAndOverflow(t *testing.T) {
	_, err := Compile(Spec{Length: 8, Suffix: ".com", Pattern: "a"}, 1_000_000_000)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("8 alnum with 1e9 cap: err = %v, want ErrTooLarge", err)
	}
	_, err = Compile(Spec{Length: 30, Suffix: ".com", Pattern: "a"}, noLimit)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("overflowing space: err = %v, want ErrTooLarge (not a negative total)", err)
	}
	if _, err := Compile(Spec{Length: 64, Suffix: ".com", Pattern: "d"}, noLimit); err == nil {
		t.Fatalf("label longer than 63 must be rejected")
	}
}

func TestDictionaryNormalisation(t *testing.T) {
	words := []string{"Hello", "wo_rld", "ok", "-bad", "Ünï", strings.Repeat("a", 64)}
	p := mustCompile(t, Spec{Suffix: ".li", Words: words})
	if p.Total() != 6 {
		t.Fatalf("Total = %d, want 6", p.Total())
	}
	want := []struct {
		d  string
		ok bool
	}{{"hello.li", true}, {"", false}, {"ok.li", true}, {"", false}, {"", false}, {"", false}}
	for i, w := range want {
		d, ok := p.At(int64(i))
		if ok != w.ok || (ok && d != w.d) {
			t.Errorf("At(%d) = %q,%v want %q,%v", i, d, ok, w.d, w.ok)
		}
	}
}

func TestEmptyDictionaryRejected(t *testing.T) {
	if _, err := Compile(Spec{Suffix: ".li", Words: []string{}}, noLimit); err == nil {
		t.Fatalf("empty dictionary must be rejected")
	}
}

func TestAtOutOfRange(t *testing.T) {
	p := mustCompile(t, Spec{Length: 1, Suffix: ".li", Pattern: "d"})
	if _, ok := p.At(10); ok {
		t.Fatalf("At beyond Total must report !ok")
	}
	if _, ok := p.At(-1); ok {
		t.Fatalf("At(-1) must report !ok")
	}
}
