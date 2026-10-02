package wordlists

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setup(t *testing.T) (*Manager, string, string) {
	t.Helper()
	b, u := t.TempDir(), filepath.Join(t.TempDir(), "user")
	os.WriteFile(filepath.Join(b, "google-10000-english.txt"), []byte("the\nof\nAnd\n\nthe\n"), 0o644)
	return NewManager(b, u), b, u
}

func TestListShowsBuiltinAndUser(t *testing.T) {
	m, _, _ := setup(t)
	if _, err := m.Save("My List", strings.NewReader("alpha\nbeta\n"), 1<<20); err != nil {
		t.Fatal(err)
	}
	list, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("list = %+v, want 2 entries", list)
	}
	var builtin, user *Info
	for i := range list {
		if list[i].Builtin {
			builtin = &list[i]
		} else {
			user = &list[i]
		}
	}
	if builtin == nil || builtin.ID != "builtin:google-10000-english" || builtin.Count != 3 {
		t.Fatalf("builtin = %+v (count must equal len(Words), i.e. unique non-empty words)", builtin)
	}
	if user == nil || user.ID != "user:my-list" || user.Count != 2 {
		t.Fatalf("user = %+v", user)
	}
}

func TestListWithMissingDirsIsEmptyNotError(t *testing.T) {
	m := NewManager(filepath.Join(t.TempDir(), "nope"), filepath.Join(t.TempDir(), "nada"))
	list, err := m.List()
	if err != nil || len(list) != 0 {
		t.Fatalf("list = %v, err = %v; want empty, nil", list, err)
	}
}

func TestWordsNormalisesAndDedupes(t *testing.T) {
	m, _, _ := setup(t)
	words, err := m.Words("builtin:google-10000-english")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"the", "of", "and"}
	if strings.Join(words, ",") != strings.Join(want, ",") {
		t.Fatalf("words = %v, want %v", words, want)
	}
}

func TestWordsRejectsTraversalAndUnknown(t *testing.T) {
	m, _, _ := setup(t)
	for _, id := range []string{
		"builtin:../../etc/passwd", "user:../x", "builtin:/etc/passwd", `builtin:..\x`,
		"builtin:", "nope:abc", "abc", "builtin:missing", "user:a/b",
	} {
		if _, err := m.Words(id); err == nil {
			t.Errorf("Words(%q) should fail", id)
		}
	}
}

func TestSaveEnforcesSizeAndLeavesNoFile(t *testing.T) {
	m, _, u := setup(t)
	_, err := m.Save("big", strings.NewReader(strings.Repeat("abcdef\n", 100)), 50)
	if err == nil {
		t.Fatal("oversize upload must fail")
	}
	if entries, _ := os.ReadDir(u); len(entries) != 0 {
		t.Fatalf("oversize upload left files behind: %v", entries)
	}
}

func TestSaveSanitisesName(t *testing.T) {
	m, _, u := setup(t)
	info, err := m.Save("../../Evil Name!!.txt", strings.NewReader("x\n"), 100)
	if err != nil {
		t.Fatal(err)
	}
	if info.ID != "user:evil-name" {
		t.Fatalf("id = %q, want user:evil-name", info.ID)
	}
	entries, _ := os.ReadDir(u)
	if len(entries) != 1 || entries[0].Name() != "evil-name.txt" {
		t.Fatalf("files = %v", entries)
	}
}

func TestSaveRejectsEmptyContentAndName(t *testing.T) {
	m, _, _ := setup(t)
	if _, err := m.Save("empty", strings.NewReader("\n  \n"), 100); err == nil {
		t.Fatal("empty content must fail")
	}
	if _, err := m.Save("!!!", strings.NewReader("a\n"), 100); err == nil {
		t.Fatal("name that sanitises to nothing must fail")
	}
}
