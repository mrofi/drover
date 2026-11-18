package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
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
	Files int `json:"files,omitempty"`
	// ID tells apart two folder versions with the same name (one deleted, another added, or a backup of
	// another drover restored): it names the version's unpacked folder in the cache.
	ID      string    `json:"id,omitempty"`
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
//
// Everything under <dir> is a plain file, so a copy of the data directory by any tool (a volume backup,
// rclone to S3) is enough to bring every version back, even one that keeps neither file modes nor links.
// A folder version is kept there as one archive, tree.tar.gz, and unpacked into the cache when it runs.
type releaseStore struct {
	dir     string
	cache   string // <cache>/<app>: unpacked folder versions, rebuilt from their archives when missing
	binName string
	builtin string
	folder  bool
	mu      sync.Mutex // one unpacking at a time
}

const (
	// treeName is the folder a folder version is unpacked into while it is being added.
	treeName = "tree"
	// archiveName is the file that keeps a folder version in the data directory.
	archiveName = "tree.tar.gz"
)

func newReleaseStore(dataDir, cacheDir string, cfg *AppConfig) (*releaseStore, error) {
	s := &releaseStore{dir: filepath.Join(dataDir, cfg.Name), cache: filepath.Join(cacheDir, cfg.Name),
		binName: "app", builtin: cfg.Binary}
	if cfg.Binary != "" {
		s.binName = filepath.Base(cfg.Binary)
	}
	if cfg.folderMode {
		s.folder, s.builtin, s.binName = true, cfg.Folder, archiveName
	}
	if err := os.MkdirAll(filepath.Join(s.dir, "releases"), 0o755); err != nil {
		return nil, err
	}
	if s.folder {
		s.migrate()
	}
	return s, nil
}

func newID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *releaseStore) meta(version string) Release {
	var rel Release
	if raw, err := os.ReadFile(filepath.Join(s.dir, "releases", version, "meta.json")); err == nil {
		_ = json.Unmarshal(raw, &rel)
	}
	return rel
}

func (s *releaseStore) treePath(version string) string {
	return filepath.Join(s.cache, version+"-"+s.meta(version).ID)
}

// migrate turns folder versions that drover 0.3 kept unpacked in the data directory into archives.
func (s *releaseStore) migrate() {
	entries, _ := os.ReadDir(filepath.Join(s.dir, "releases"))
	for _, e := range entries {
		dir := filepath.Join(s.dir, "releases", e.Name())
		tree := filepath.Join(dir, treeName)
		if st, err := os.Stat(tree); err != nil || !st.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		err := func() error {
			f, err := os.CreateTemp(dir, ".archive-")
			if err != nil {
				return err
			}
			defer os.Remove(f.Name())
			if err := tarGzDir(tree, f); err != nil {
				f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
			rel := s.meta(e.Name())
			if rel.ID == "" {
				rel.ID = newID()
				raw, _ := json.Marshal(rel)
				if err := writeFileAtomic(filepath.Join(dir, "meta.json"), raw, 0o644); err != nil {
					return err
				}
			}
			if err := os.Rename(f.Name(), filepath.Join(dir, archiveName)); err != nil {
				return err
			}
			return os.RemoveAll(tree)
		}()
		if err != nil {
			log.Printf("[%s] version %s could not be moved to an archive: %v", filepath.Base(s.dir), e.Name(), err)
		}
	}
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
	if !s.exists(version) {
		return "", errNotFound
	}
	p := filepath.Join(s.dir, "releases", version, s.binName)
	if !s.folder {
		// A copy that does not keep file modes (rclone to S3, say) loses the executable bit.
		if st, err := os.Stat(p); err == nil && st.Mode().Perm()&0o100 == 0 {
			if err := os.Chmod(p, st.Mode().Perm()|0o755); err != nil {
				return "", err
			}
		}
		return p, nil
	}
	return s.unpack(version)
}

// exists reports whether a stored version is there, without unpacking it.
func (s *releaseStore) exists(version string) bool {
	if !nameRe.MatchString(version) || version == versionBuiltin {
		return false
	}
	_, err := os.Stat(filepath.Join(s.dir, "releases", version, s.binName))
	return err == nil
}

// unpack returns the unpacked folder of a folder version, unpacking its archive into the cache first when
// it is not there (a new container, a cleared cache, a restored backup).
func (s *releaseStore) unpack(version string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tree := s.treePath(version)
	if st, err := os.Stat(tree); err == nil && st.IsDir() {
		return tree, nil
	}
	if err := os.MkdirAll(s.cache, 0o755); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(s.cache, ".unpack-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	if err := extractArchive(filepath.Join(s.dir, "releases", version, archiveName), tmp, "", math.MaxInt64); err != nil {
		return "", fmt.Errorf("version %s: %w", version, err)
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, tree); err != nil {
		return "", err
	}
	return tree, nil
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
		tree := filepath.Join(tmp, treeName)
		if rel.Files, rel.Size, err = checkTree(tree, maxBytes); err != nil {
			return Release{}, fmt.Errorf("the release cannot be used: %w", err)
		}
		if err := s.archive(tree, filepath.Join(tmp, archiveName)); err != nil {
			return Release{}, fmt.Errorf("keeping the release: %w", err)
		}
		rel.ID = newID()
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
	// The unpacked folder goes to the cache, ready to run. Where the cache is on another file system the
	// rename fails, and the folder is unpacked from the archive the first time it runs instead.
	moved := ""
	if s.folder {
		tree, cached := filepath.Join(tmp, treeName), filepath.Join(s.cache, version+"-"+rel.ID)
		if os.MkdirAll(s.cache, 0o755) == nil && os.Rename(tree, cached) == nil {
			moved = cached
		} else if err := os.RemoveAll(tree); err != nil {
			return Release{}, err
		}
	}
	if err := os.Rename(tmp, final); err != nil {
		if moved != "" {
			os.RemoveAll(moved)
		}
		if _, serr := os.Stat(final); serr == nil {
			return Release{}, errExists
		}
		return Release{}, err
	}
	return rel, nil
}

// archive writes tree as a tar.gz, the form a folder version is kept in.
func (s *releaseStore) archive(tree, dest string) error {
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if err := tarGzDir(tree, f); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func (s *releaseStore) remove(version string) error {
	if !s.exists(version) {
		return errNotFound
	}
	if s.folder {
		if err := os.RemoveAll(s.treePath(version)); err != nil {
			return err
		}
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
