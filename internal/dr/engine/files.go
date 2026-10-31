package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const (
	dirMode  = 0o700
	fileMode = 0o600
	// maxNameLength keeps escaped file names far below the usual 255 byte limit.
	maxNameLength = 150
)

// escapeName turns any string into a file name that cannot escape its directory: letters, digits,
// '_' and '-' are kept, every other byte (including '.', '/' and '\') becomes %XX. There are no dots
// at all, so neither ".." nor hidden or absolute names can come out of it. Long names are cut and
// get a hash suffix so two long keys never collide.
func escapeName(name string) string {
	var b strings.Builder
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	escaped := b.String()
	if len(escaped) <= maxNameLength {
		return escaped
	}
	sum := sha256.Sum256([]byte(name))
	return escaped[:100] + "~" + hex.EncodeToString(sum[:16])
}

// writeFileAtomic writes data so a crash leaves either the old file or the new one, never half of it.
func writeFileAtomic(target string, data []byte) error {
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	cleanup := func() { _ = os.Remove(tmp.Name()) }

	if err := tmp.Chmod(fileMode); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmp.Name(), target); err != nil {
		cleanup()
		return err
	}
	syncDir(dir)
	return nil
}

// syncDir makes a rename durable where the platform supports it; failures are not actionable.
func syncDir(dir string) {
	// #nosec G304 -- dir is the engine's own directory.
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
}

// ---- FileState ------------------------------------------------------------------------------------------------

// FileState is a dr.StateStore that keeps one JSON file per key under <dir>/state.
type FileState struct {
	dir string
	mu  sync.Mutex
}

// NewFileState returns a store rooted at dir (usually <engine dir>/state). The directory is created
// on the first Save.
func NewFileState(dir string) *FileState { return &FileState{dir: dir} }

func (s *FileState) file(key string) (string, error) {
	if key == "" {
		return "", errors.New("state key must not be empty")
	}
	return filepath.Join(s.dir, escapeName(key)+".json"), nil
}

// Load reads the document stored under key into into.
func (s *FileState) Load(_ context.Context, key string, into any) (bool, error) {
	file, err := s.file(key)
	if err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// #nosec G304 -- file is under the state directory and its name is escaped.
	data, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(data, into); err != nil {
		return false, fmt.Errorf("state %q is corrupt: %w", key, err)
	}
	return true, nil
}

// Save stores value under key, replacing what was there.
func (s *FileState) Save(_ context.Context, key string, value any) error {
	file, err := s.file(key)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("cannot encode state %q: %w", key, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return writeFileAtomic(file, data)
}

// Delete removes key; a missing key is not an error.
func (s *FileState) Delete(_ context.Context, key string) error {
	file, err := s.file(key)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(file); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// ---- MemoryState ----------------------------------------------------------------------------------------------

// MemoryState is an in-memory dr.StateStore for tests. Like the file store it keeps JSON, so what
// comes back is a copy and not the caller's own value.
type MemoryState struct {
	mu   sync.Mutex
	data map[string][]byte
}

// NewMemoryState returns an empty store.
func NewMemoryState() *MemoryState { return &MemoryState{data: map[string][]byte{}} }

// Load implements dr.StateStore.
func (s *MemoryState) Load(_ context.Context, key string, into any) (bool, error) {
	if key == "" {
		return false, errors.New("state key must not be empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.data[key]
	if !ok {
		return false, nil
	}
	if err := json.Unmarshal(data, into); err != nil {
		return false, fmt.Errorf("state %q is corrupt: %w", key, err)
	}
	return true, nil
}

// Save implements dr.StateStore.
func (s *MemoryState) Save(_ context.Context, key string, value any) error {
	if key == "" {
		return errors.New("state key must not be empty")
	}
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("cannot encode state %q: %w", key, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[key] = data
	return nil
}

// Delete implements dr.StateStore.
func (s *MemoryState) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, key)
	return nil
}
