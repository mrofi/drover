package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// versionBuiltin names the binary that came with the image (AppConfig.Binary), as opposed to a stored release.
const versionBuiltin = "builtin"

var (
	errExists   = errors.New("that version already exists")
	errNotFound = errors.New("no such version")
)

// Release describes one stored version of an app's binary.
type Release struct {
	Version string    `json:"version"`
	Size    int64     `json:"size"`
	SHA256  string    `json:"sha256,omitempty"`
	Source  string    `json:"source,omitempty"`
	Created time.Time `json:"created"`
	Current bool      `json:"current"`
}

type releaseState struct {
	Current  string `json:"current"`
	Previous string `json:"previous"`
}

// releaseStore keeps an app's releases on disk: <dir>/releases/<version>/<binary name> plus meta.json,
// and <dir>/state.json with the current and previous version.
type releaseStore struct {
	dir     string
	binName string
	builtin string
}

func newReleaseStore(dataDir string, cfg *AppConfig) (*releaseStore, error) {
	s := &releaseStore{dir: filepath.Join(dataDir, cfg.Name), binName: "app", builtin: cfg.Binary}
	if cfg.Binary != "" {
		s.binName = filepath.Base(cfg.Binary)
	}
	return s, os.MkdirAll(filepath.Join(s.dir, "releases"), 0o755)
}

func (s *releaseStore) hasBuiltin() bool {
	if s.builtin == "" {
		return false
	}
	st, err := os.Stat(s.builtin)
	return err == nil && !st.IsDir()
}

// binPath returns the file to execute for a version.
func (s *releaseStore) binPath(version string) (string, error) {
	if version == versionBuiltin {
		if !s.hasBuiltin() {
			return "", fmt.Errorf("no built-in binary at %q", s.builtin)
		}
		return s.builtin, nil
	}
	if !nameRe.MatchString(version) {
		return "", fmt.Errorf("invalid version %q", version)
	}
	p := filepath.Join(s.dir, "releases", version, s.binName)
	if _, err := os.Stat(p); err != nil {
		return "", errNotFound
	}
	return p, nil
}

func (s *releaseStore) loadState() releaseState {
	st := releaseState{Current: versionBuiltin}
	raw, err := os.ReadFile(filepath.Join(s.dir, "state.json"))
	if err == nil {
		_ = json.Unmarshal(raw, &st)
	}
	if st.Current == "" {
		st.Current = versionBuiltin
	}
	return st
}

func (s *releaseStore) saveState(st releaseState) error {
	raw, _ := json.Marshal(st)
	return writeFileAtomic(filepath.Join(s.dir, "state.json"), raw, 0o644)
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// add stores r as a new version. The file is written next to its final place and renamed in, so a half
// written release is never visible. wantSHA, when set, must match the content.
func (s *releaseStore) add(version, source string, r io.Reader, wantSHA string, maxBytes int64) (Release, error) {
	if version == versionBuiltin || !nameRe.MatchString(version) {
		return Release{}, fmt.Errorf("invalid version %q", version)
	}
	final := filepath.Join(s.dir, "releases", version)
	if _, err := os.Stat(final); err == nil {
		return Release{}, errExists
	}
	tmp, err := os.MkdirTemp(filepath.Join(s.dir, "releases"), ".incoming-")
	if err != nil {
		return Release{}, err
	}
	defer os.RemoveAll(tmp)
	f, err := os.OpenFile(filepath.Join(tmp, s.binName), os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o755)
	if err != nil {
		return Release{}, err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(r, maxBytes+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return Release{}, fmt.Errorf("receiving the binary: %w", err)
	}
	if n == 0 {
		return Release{}, errors.New("the binary is empty")
	}
	if n > maxBytes {
		return Release{}, fmt.Errorf("the binary is larger than max_binary_bytes (%d)", maxBytes)
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if wantSHA != "" && !strings.EqualFold(wantSHA, sum) {
		return Release{}, fmt.Errorf("sha256 mismatch: got %s, want %s", sum, wantSHA)
	}
	rel := Release{Version: version, Size: n, SHA256: sum, Source: source, Created: time.Now().UTC()}
	meta, _ := json.Marshal(rel)
	if err := os.WriteFile(filepath.Join(tmp, "meta.json"), meta, 0o644); err != nil {
		return Release{}, err
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		return Release{}, err
	}
	if err := os.Rename(tmp, final); err != nil {
		if _, serr := os.Stat(final); serr == nil {
			return Release{}, errExists
		}
		return Release{}, err
	}
	return rel, nil
}

func (s *releaseStore) remove(version string) error {
	if _, err := s.binPath(version); err != nil {
		return err
	}
	return os.RemoveAll(filepath.Join(s.dir, "releases", version))
}

// list returns the built-in binary (when there is one) followed by the stored releases, oldest first.
func (s *releaseStore) list(current string) []Release {
	out := []Release{}
	if s.hasBuiltin() {
		st, _ := os.Stat(s.builtin)
		out = append(out, Release{Version: versionBuiltin, Size: st.Size(), Source: s.builtin,
			Created: st.ModTime().UTC(), Current: current == versionBuiltin})
	}
	entries, _ := os.ReadDir(filepath.Join(s.dir, "releases"))
	stored := []Release{}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		rel := Release{Version: e.Name()}
		if raw, err := os.ReadFile(filepath.Join(s.dir, "releases", e.Name(), "meta.json")); err == nil {
			_ = json.Unmarshal(raw, &rel)
		}
		rel.Version = e.Name()
		rel.Current = current == rel.Version
		stored = append(stored, rel)
	}
	sort.Slice(stored, func(i, j int) bool { return stored[i].Created.Before(stored[j].Created) })
	return append(out, stored...)
}
