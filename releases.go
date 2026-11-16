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
	Version string `json:"version"`
	Size    int64  `json:"size"`
	// Files is the number of files in a folder release.
	Files   int       `json:"files,omitempty"`
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
	folder  bool // a version is a folder (releases/<version>/tree), not one file
}

// treeName is the folder inside a version's directory that holds the files of a folder release.
const treeName = "tree"

func newReleaseStore(dataDir string, cfg *AppConfig) (*releaseStore, error) {
	s := &releaseStore{dir: filepath.Join(dataDir, cfg.Name), binName: "app", builtin: cfg.Binary}
	if cfg.Binary != "" {
		s.binName = filepath.Base(cfg.Binary)
	}
	if cfg.folderMode {
		s.folder, s.builtin, s.binName = true, cfg.Folder, treeName
	}
	return s, os.MkdirAll(filepath.Join(s.dir, "releases"), 0o755)
}

func (s *releaseStore) hasBuiltin() bool {
	if s.builtin == "" {
		return false
	}
	st, err := os.Stat(s.builtin)
	return err == nil && st.IsDir() == s.folder
}

// binPath returns what a version runs from: the binary of a file app, the folder of a folder app.
func (s *releaseStore) binPath(version string) (string, error) {
	if version == versionBuiltin {
		if !s.hasBuiltin() {
			return "", fmt.Errorf("no built-in %s at %q", map[bool]string{false: "binary", true: "folder"}[s.folder], s.builtin)
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

// add stores r as a new version: the binary itself for a file app, or an archive (tar, tar.gz, tar.bz2,
// zip) that is unpacked for a folder app, keeping only what is under subdir. wantSHA, when set, must match
// what was received.
func (s *releaseStore) add(version, source string, r io.Reader, wantSHA string, maxBytes int64, subdir string) (Release, error) {
	if !s.folder && subdir != "" {
		return Release{}, errors.New("subdir is only for folder apps (a command with {dir}, or folder: in the config)")
	}
	return s.addWith(version, maxBytes, func(tmp string) (string, string, error) {
		name := s.binName
		if s.folder {
			name = ".archive"
		}
		f, err := os.OpenFile(filepath.Join(tmp, name), os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o755)
		if err != nil {
			return "", "", err
		}
		h := sha256.New()
		n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(r, maxBytes+1))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		what := map[bool]string{false: "binary", true: "archive"}[s.folder]
		switch {
		case err != nil:
			return "", "", fmt.Errorf("receiving the %s: %w", what, err)
		case n == 0:
			return "", "", fmt.Errorf("the %s is empty", what)
		case n > maxBytes:
			return "", "", fmt.Errorf("the %s is larger than max_binary_bytes (%d)", what, maxBytes)
		}
		sum := hex.EncodeToString(h.Sum(nil))
		if wantSHA != "" && !strings.EqualFold(wantSHA, sum) {
			return "", "", fmt.Errorf("sha256 mismatch: got %s, want %s", sum, wantSHA)
		}
		if !s.folder {
			return sum, source, nil
		}
		tree := filepath.Join(tmp, treeName)
		if err := os.Mkdir(tree, 0o755); err != nil {
			return "", "", err
		}
		if err := extractArchive(f.Name(), tree, subdir, maxBytes); err != nil {
			return "", "", err
		}
		return sum, source, os.Remove(f.Name())
	})
}

// addFolder stores a version that fill writes as a folder (a git checkout): fill puts the files in the
// directory it is given and returns what to record as the source.
func (s *releaseStore) addFolder(version string, maxBytes int64, fill func(tree string) (source string, err error)) (Release, error) {
	if !s.folder {
		return Release{}, errors.New("this source gives a folder, and the app runs a single binary (use {dir} in its command, or folder: in the config)")
	}
	return s.addWith(version, maxBytes, func(tmp string) (string, string, error) {
		source, err := fill(filepath.Join(tmp, treeName))
		return "", source, err
	})
}

// addWith creates a version: fill writes its content into a temporary directory and says what it was (a
// checksum, where it came from). The directory is renamed into place only when everything succeeded, so a
// half written release is never visible.
func (s *releaseStore) addWith(version string, maxBytes int64, fill func(tmp string) (sha, source string, err error)) (Release, error) {
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
	sum, source, err := fill(tmp)
	if err != nil {
		return Release{}, err
	}
	rel := Release{Version: version, SHA256: sum, Source: source, Created: time.Now().UTC()}
	if s.folder {
		if rel.Files, rel.Size, err = checkTree(filepath.Join(tmp, treeName), maxBytes); err != nil {
			return Release{}, fmt.Errorf("the release cannot be used: %w", err)
		}
	} else if st, err := os.Stat(filepath.Join(tmp, s.binName)); err == nil {
		rel.Size = st.Size()
	}
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
		size := st.Size()
		if s.folder {
			size = 0 // not measured: it may be large, and it is not drover's
		}
		out = append(out, Release{Version: versionBuiltin, Size: size, Source: s.builtin,
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
