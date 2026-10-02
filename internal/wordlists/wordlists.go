// Package wordlists serves dictionary files: built-in lists baked into the image and lists
// uploaded by the user.
package wordlists

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

type Info struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Source  string `json:"source"`
	Count   int    `json:"count"`
	Builtin bool   `json:"builtin"`
}

type Manager struct {
	builtinDir, userDir string

	mu    sync.Mutex
	cache map[string]cachedCount
}

type cachedCount struct {
	mod   time.Time
	size  int64
	count int
}

var (
	idNameRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	unsafeChar = regexp.MustCompile(`[^a-z0-9._-]+`)
)

// Source URLs for the built-in lists (fetched at image build time by scripts/fetch-wordlists.sh).
var builtinSources = map[string]string{
	"google-10000-english": "github.com/first20hours/google-10000-english",
	"english-words-alpha":  "github.com/dwyl/english-words",
}

func NewManager(builtinDir, userDir string) *Manager {
	return &Manager{builtinDir: builtinDir, userDir: userDir, cache: map[string]cachedCount{}}
}

// List returns all available lists, built-in first, each group sorted by name.
func (m *Manager) List() ([]Info, error) {
	var out []Info
	for _, g := range []struct {
		dir, prefix string
		builtin     bool
	}{{m.builtinDir, "builtin", true}, {m.userDir, "user", false}} {
		entries, err := os.ReadDir(g.dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var group []Info
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".txt") {
				continue
			}
			name := strings.TrimSuffix(e.Name(), ".txt")
			if !idNameRe.MatchString(name) {
				continue
			}
			count, err := m.count(filepath.Join(g.dir, e.Name()))
			if err != nil {
				continue
			}
			info := Info{ID: g.prefix + ":" + name, Name: name, Count: count, Builtin: g.builtin}
			if g.builtin {
				info.Source = builtinSources[name]
			} else {
				info.Source = "upload"
			}
			group = append(group, info)
		}
		sort.Slice(group, func(i, j int) bool { return group[i].Name < group[j].Name })
		out = append(out, group...)
	}
	return out, nil
}

func (m *Manager) count(path string) (int, error) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	m.mu.Lock()
	c, ok := m.cache[path]
	m.mu.Unlock()
	if ok && c.mod.Equal(st.ModTime()) && c.size == st.Size() {
		return c.count, nil
	}
	words, err := readWords(path)
	if err != nil {
		return 0, err
	}
	m.mu.Lock()
	m.cache[path] = cachedCount{mod: st.ModTime(), size: st.Size(), count: len(words)}
	m.mu.Unlock()
	return len(words), nil
}

// Words returns the normalised (trimmed, lower-cased, de-duplicated) words of list id.
func (m *Manager) Words(id string) ([]string, error) {
	path, err := m.path(id)
	if err != nil {
		return nil, err
	}
	words, err := readWords(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("wordlist %q not found", id)
	}
	return words, err
}

func (m *Manager) path(id string) (string, error) {
	prefix, name, ok := strings.Cut(id, ":")
	if !ok || !idNameRe.MatchString(name) {
		return badID("invalid wordlist id %q", id)
	}
	switch prefix {
	case "builtin":
		return filepath.Join(m.builtinDir, name+".txt"), nil
	case "user":
		return filepath.Join(m.userDir, name+".txt"), nil
	}
	return badID("invalid wordlist id %q", id)
}

func badID(format string, args ...any) (string, error) { return "", fmt.Errorf(format, args...) }

// Save stores an uploaded list. The name is sanitised; content larger than maxBytes is rejected.
func (m *Manager) Save(name string, r io.Reader, maxBytes int64) (Info, error) {
	clean := sanitiseName(name)
	if clean == "" {
		return Info{}, errors.New("invalid wordlist name")
	}
	data, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return Info{}, err
	}
	if int64(len(data)) > maxBytes {
		return Info{}, fmt.Errorf("wordlist too large (max %d bytes)", maxBytes)
	}
	words := normalise(strings.NewReader(string(data)))
	if len(words) == 0 {
		return Info{}, errors.New("wordlist is empty")
	}
	if err := os.MkdirAll(m.userDir, 0o755); err != nil {
		return Info{}, err
	}
	final := filepath.Join(m.userDir, clean+".txt")
	tmp, err := os.CreateTemp(m.userDir, ".upload-*")
	if err != nil {
		return Info{}, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(strings.Join(words, "\n") + "\n"); err != nil {
		tmp.Close()
		return Info{}, err
	}
	if err := tmp.Close(); err != nil {
		return Info{}, err
	}
	if err := os.Rename(tmp.Name(), final); err != nil {
		return Info{}, err
	}
	return Info{ID: "user:" + clean, Name: clean, Source: "upload", Count: len(words)}, nil
}

func sanitiseName(name string) string {
	name = strings.ToLower(filepath.Base(strings.ReplaceAll(strings.TrimSpace(name), `\`, "/")))
	name = strings.TrimSuffix(name, ".txt")
	name = strings.Trim(unsafeChar.ReplaceAllString(name, "-"), "-._")
	if len(name) > 64 {
		name = name[:64]
	}
	if !idNameRe.MatchString(name) {
		return ""
	}
	return name
}

func readWords(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return normalise(f), nil
}

func normalise(r io.Reader) []string {
	seen := map[string]struct{}{}
	var out []string
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		w := strings.ToLower(strings.TrimSpace(sc.Text()))
		if w == "" {
			continue
		}
		if _, dup := seen[w]; dup {
			continue
		}
		seen[w] = struct{}{}
		out = append(out, w)
	}
	return out
}
