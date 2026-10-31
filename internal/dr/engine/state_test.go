package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type doc struct {
	Replicas map[string]int `json:"replicas"`
	Note     string         `json:"note"`
}

func TestFileStateRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	s := NewFileState(dir)
	ctx := context.Background()

	var got doc
	if found, err := s.Load(ctx, "kind-lab/apps", &got); err != nil || found {
		t.Fatalf("missing key: %v %v", found, err)
	}

	want := doc{Replicas: map[string]int{"shop/api": 3}, Note: "before the shutdown"}
	if err := s.Save(ctx, "kind-lab/apps", want); err != nil {
		t.Fatal(err)
	}
	if found, err := s.Load(ctx, "kind-lab/apps", &got); err != nil || !found || got.Replicas["shop/api"] != 3 || got.Note != want.Note {
		t.Fatalf("load = %+v %v %v", got, found, err)
	}

	want.Replicas["shop/api"] = 5
	if err := s.Save(ctx, "kind-lab/apps", want); err != nil {
		t.Fatal(err)
	}
	var again doc
	if _, err := s.Load(ctx, "kind-lab/apps", &again); err != nil || again.Replicas["shop/api"] != 5 {
		t.Errorf("overwrite = %+v %v", again, err)
	}

	if err := s.Delete(ctx, "kind-lab/apps"); err != nil {
		t.Fatal(err)
	}
	if found, _ := s.Load(ctx, "kind-lab/apps", &got); found {
		t.Error("deleted key still there")
	}
	if err := s.Delete(ctx, "kind-lab/apps"); err != nil {
		t.Errorf("deleting a missing key: %v", err)
	}
}

func TestFileStatePermissionsAndAtomicity(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	s := NewFileState(dir)
	if err := s.Save(context.Background(), "k", doc{Note: "x"}); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(dir); err != nil || st.Mode().Perm() != 0o700 {
		t.Errorf("dir mode: %v %v", st, err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || strings.HasPrefix(entries[0].Name(), ".tmp-") {
		t.Fatalf("entries = %v", entries)
	}
	if st, _ := entries[0].Info(); st.Mode().Perm() != 0o600 {
		t.Errorf("file mode = %v", st.Mode().Perm())
	}
}

func TestFileStateKeysCannotTraverse(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "state")
	s := NewFileState(dir)
	ctx := context.Background()

	for _, key := range []string{
		"../../escape", "../escape", "/etc/passwd", "a/../../b", "..", ".", "...", `..\..\x`, "a\x00b",
		"arn:aws:eks:us-east-1:123:cluster/prod/apps", strings.Repeat("long/", 100),
	} {
		if err := s.Save(ctx, key, doc{Note: key}); err != nil {
			t.Errorf("Save(%q): %v", key, err)
			continue
		}
		var back doc
		if found, err := s.Load(ctx, key, &back); err != nil || !found || back.Note != key {
			t.Errorf("Load(%q) = %+v %v %v", key, back, found, err)
		}
	}

	// Nothing was written outside the state directory.
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && filepath.Dir(p) != dir {
			t.Errorf("file outside the state directory: %s", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := s.Save(ctx, "", doc{}); err == nil {
		t.Error("an empty key must be refused")
	}
	if _, err := s.Load(ctx, "", &doc{}); err == nil {
		t.Error("an empty key must be refused on load")
	}
	if err := s.Delete(ctx, ""); err == nil {
		t.Error("an empty key must be refused on delete")
	}
}

func TestFileStateDetectsCorruptionAndDistinguishesKeys(t *testing.T) {
	dir := t.TempDir()
	s := NewFileState(dir)
	ctx := context.Background()

	if err := s.Save(ctx, "a/b", doc{Note: "slash"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(ctx, "a%2Fb", doc{Note: "percent"}); err != nil {
		t.Fatal(err)
	}
	var one, two doc
	_, _ = s.Load(ctx, "a/b", &one)
	_, _ = s.Load(ctx, "a%2Fb", &two)
	if one.Note != "slash" || two.Note != "percent" {
		t.Errorf("keys collided: %q %q", one.Note, two.Note)
	}

	file, _ := s.file("a/b")
	if err := os.WriteFile(file, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if found, err := s.Load(ctx, "a/b", &one); err == nil || found {
		t.Errorf("a corrupt document must be an error, got %v %v", found, err)
	}
}

func TestLongKeysDoNotCollide(t *testing.T) {
	base := strings.Repeat("x", 300)
	a, b := escapeName(base+"1"), escapeName(base+"2")
	if a == b || len(a) > 160 || len(b) > 160 {
		t.Errorf("long names: %d %d chars, equal=%v", len(a), len(b), a == b)
	}
	if escapeName("a.b") == "a.b" || strings.ContainsAny(escapeName("../x"), "./") {
		t.Error("dots and slashes must be escaped")
	}
	if escapeName("kind-lab_1") != "kind-lab_1" {
		t.Error("plain names must stay readable")
	}
}

func TestMemoryStateBehavesLikeTheFileStore(t *testing.T) {
	s := NewMemoryState()
	ctx := context.Background()

	original := doc{Replicas: map[string]int{"a": 1}}
	if err := s.Save(ctx, "k", original); err != nil {
		t.Fatal(err)
	}
	original.Replicas["a"] = 99 // changing the caller's value must not change the stored one

	var back doc
	if found, err := s.Load(ctx, "k", &back); err != nil || !found || back.Replicas["a"] != 1 {
		t.Errorf("stored = %+v %v %v", back, found, err)
	}
	if found, _ := s.Load(ctx, "nope", &back); found {
		t.Error("unknown key found")
	}
	if err := s.Delete(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	if found, _ := s.Load(ctx, "k", &back); found {
		t.Error("deleted key found")
	}
	if err := s.Save(ctx, "", 1); err == nil {
		t.Error("an empty key must be refused")
	}
	if err := s.Save(ctx, "bad", make(chan int)); err == nil {
		t.Error("an unencodable value must be an error")
	}
	if _, err := s.Load(ctx, "", &back); err == nil {
		t.Error("an empty key must be refused on load")
	}
	_ = s.Save(ctx, "num", 5)
	var wrong doc
	if _, err := s.Load(ctx, "num", &wrong); err == nil {
		t.Error("loading into the wrong type must be an error")
	}
}
