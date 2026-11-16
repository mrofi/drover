package main

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"regexp"
	"sort"
	"strings"
)

// An instance's environment is built from four layers, later ones winning:
//
//  1. drover's own environment (without DROVER_TOKEN)
//  2. env: in the config
//  3. the app's env_file, read every time an instance is started
//  4. overrides set through the API, kept in memory only
//
// and then PORT and DROVER_*, which drover always sets itself.

var (
	envKeyRe      = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	errNoEnvFile  = errors.New("this app has no env_file configured")
	errRolledBack = errors.New("the instances did not come up with the new environment, so the change was undone")
)

// Variables drover sets for every instance. They cannot be changed from a file or the API.
var reservedEnv = map[string]bool{"PORT": true, "DROVER_APP": true, "DROVER_INDEX": true,
	"DROVER_VERSION": true, "DROVER_TOKEN": true, "DROVER_DIR": true}

// envChange is a set of edits: variables to set and variables to remove.
type envChange struct {
	Set   map[string]string `json:"set"`
	Unset []string          `json:"unset"`
}

func checkEnvKey(k string) error {
	if !envKeyRe.MatchString(k) {
		return fmt.Errorf("invalid variable name %q", k)
	}
	if reservedEnv[k] {
		return fmt.Errorf("%s is set by drover and cannot be changed", k)
	}
	return nil
}

func (c envChange) validate() error {
	if len(c.Set) == 0 && len(c.Unset) == 0 {
		return errors.New(`nothing to change: give "set" and/or "unset"`)
	}
	for k, v := range c.Set {
		if err := checkEnvKey(k); err != nil {
			return err
		}
		if strings.ContainsRune(v, 0) {
			return fmt.Errorf("the value of %s contains a NUL byte", k)
		}
	}
	for _, k := range c.Unset {
		if err := checkEnvKey(k); err != nil {
			return err
		}
	}
	return nil
}

// parseEnvLine reads one line of an env file: KEY=VALUE, optionally after "export ". Blank lines and lines
// starting with # are skipped (ok false). A value may be in single quotes (taken literally) or double
// quotes (\n, \" and \\ are unescaped); otherwise it is the rest of the line, trimmed.
func parseEnvLine(line string) (key, value string, ok bool, err error) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false, nil
	}
	line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
	i := strings.IndexByte(line, '=')
	if i < 0 {
		return "", "", false, errors.New("expected KEY=VALUE")
	}
	key, value = strings.TrimSpace(line[:i]), strings.TrimSpace(line[i+1:])
	if !envKeyRe.MatchString(key) {
		return "", "", false, fmt.Errorf("invalid variable name %q", key)
	}
	if n := len(value); n >= 2 {
		switch {
		case value[0] == '\'' && value[n-1] == '\'':
			value = value[1 : n-1]
		case value[0] == '"' && value[n-1] == '"':
			value = strings.NewReplacer(`\n`, "\n", `\"`, `"`, `\\`, `\`).Replace(value[1 : n-1])
		}
	}
	return key, value, true, nil
}

// parseEnv returns the variables of an env file as KEY=VALUE strings in file order, and their names.
func parseEnv(data []byte) (vars, names []string, err error) {
	for n, line := range strings.Split(string(data), "\n") {
		k, v, ok, perr := parseEnvLine(line)
		if perr != nil {
			return nil, nil, fmt.Errorf("line %d: %w", n+1, perr)
		}
		if ok {
			vars = append(vars, k+"="+v)
			names = append(names, k)
		}
	}
	return vars, names, nil
}

// formatEnvLine writes a variable so that parseEnvLine reads the same value back.
func formatEnvLine(k, v string) string {
	// Anything a shell or a dotenv library might read differently is quoted.
	if v != "" && !strings.ContainsAny(v, " \t\"'\\\n#") {
		return k + "=" + v
	}
	return k + `="` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(v) + `"`
}

// editEnv applies a change to the text of an env file. A variable that is set replaces its existing line
// (or is added at the end); one that is unset loses its lines. Everything else, comments included, stays.
func editEnv(data []byte, c envChange) []byte {
	unset := map[string]bool{}
	for _, k := range c.Unset {
		unset[k] = true
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(data) == 0 {
		lines = nil
	}
	written := map[string]bool{}
	var out []string
	for _, line := range lines {
		k, _, ok, err := parseEnvLine(line)
		if !ok || err != nil {
			out = append(out, line)
			continue
		}
		if v, set := c.Set[k]; set {
			if !written[k] {
				out = append(out, formatEnvLine(k, v))
				written[k] = true
			}
			continue
		}
		if !unset[k] {
			out = append(out, line)
		}
	}
	keys := make([]string, 0, len(c.Set))
	for k := range c.Set {
		if !written[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, formatEnvLine(k, c.Set[k]))
	}
	if len(out) == 0 {
		return nil
	}
	return []byte(strings.Join(out, "\n") + "\n")
}

// readEnvFile returns the variables of the app's env_file. No env_file, or a file that does not exist yet,
// is an empty list; a file that cannot be parsed is an error, so an instance is never started on half of it.
func (a *App) readEnvFile() (vars, names []string, err error) {
	if a.cfg.EnvFile == "" {
		return nil, nil, nil
	}
	data, err := os.ReadFile(a.cfg.EnvFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("env_file: %w", err)
	}
	if vars, names, err = parseEnv(data); err != nil {
		return nil, nil, fmt.Errorf("env_file %s: %w", a.cfg.EnvFile, err)
	}
	return vars, names, nil
}

// instanceEnv is what a new instance gets on top of drover's own environment: config, file, overrides.
// PORT and DROVER_* are added by the instance itself, last. Callers hold a.mu.
func (a *App) instanceEnv(fileVars []string) []string {
	var env []string
	for k, v := range a.cfg.Env {
		env = append(env, k+"="+v)
	}
	env = append(env, fileVars...)
	for k, v := range a.overrides {
		env = append(env, k+"="+v)
	}
	return env
}

// envPending reports whether the stored environment (config, env file, overrides) differs from what a
// running instance was started with: a change was stored with restart=false, or the file was edited by
// hand, and no rollout has tried it yet. An env file that cannot be parsed counts as pending too.
func (a *App) envPending() bool {
	fileVars, _, err := a.readEnvFile()
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.envPendingLocked(fileVars, err)
}

func (a *App) envPendingLocked(fileVars []string, fileErr error) bool {
	want := envMap(a.instanceEnv(fileVars))
	for _, p := range a.slots {
		if p != nil && (fileErr != nil || !maps.Equal(want, envMap(p.env))) {
			return true
		}
	}
	return false
}

// envMap resolves a KEY=VALUE list the way the process will see it: the last one wins.
func envMap(vars []string) map[string]string {
	m := make(map[string]string, len(vars))
	for _, kv := range vars {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	return m
}

// changeEnv applies a change to the app's environment and, when restart is set, moves every instance onto
// it with a rolling restart. apply makes the change and returns how to undo it; if the new instances do
// not come up, the change is undone and the old instances keep serving.
func (a *App) changeEnv(restart bool, apply func() (undo func(), err error)) error {
	if !a.rollMu.TryLock() {
		return errBusy
	}
	defer a.rollMu.Unlock()
	undo, err := apply()
	if err != nil {
		return err
	}
	a.mu.RLock()
	current := a.version
	a.mu.RUnlock()
	if _, err := a.store.binPath(current); !restart || err != nil {
		return nil // nothing is running yet, or the caller will restart later
	}
	if err := a.rolloutLocked(current, false); err != nil {
		undo()
		return fmt.Errorf("%w: %v", errRolledBack, err)
	}
	return nil
}

// setOverrides changes the in-memory overrides. With clear, all of them are dropped first.
func (a *App) setOverrides(c envChange, clear, restart bool) error {
	return a.changeEnv(restart, func() (func(), error) {
		a.mu.Lock()
		defer a.mu.Unlock()
		before := a.overrides
		next := map[string]string{}
		if !clear {
			for k, v := range before {
				next[k] = v
			}
		}
		for _, k := range c.Unset {
			delete(next, k)
		}
		for k, v := range c.Set {
			next[k] = v
		}
		a.overrides = next
		return func() {
			a.mu.Lock()
			a.overrides = before
			a.mu.Unlock()
		}, nil
	})
}

// writeEnvFile changes the app's env_file: edit rewrites the current content into the new one.
func (a *App) writeEnvFile(restart bool, edit func(current []byte) ([]byte, error)) error {
	if a.cfg.EnvFile == "" {
		return errNoEnvFile
	}
	return a.changeEnv(restart, func() (func(), error) {
		path := a.cfg.EnvFile
		before, err := os.ReadFile(path)
		existed := err == nil
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("env_file: %w", err)
		}
		next, err := edit(before)
		if err != nil {
			return nil, err
		}
		for _, name := range mustNames(next) {
			if reservedEnv[name] {
				return nil, fmt.Errorf("%s is set by drover and cannot be changed", name)
			}
		}
		// Written in place, not renamed over: the file may be a single mounted file. New files are private.
		if err := os.WriteFile(path, next, 0o600); err != nil {
			return nil, fmt.Errorf("env_file is not writable: %w", err)
		}
		return func() {
			if existed {
				_ = os.WriteFile(path, before, 0o600)
			} else {
				_ = os.Remove(path)
			}
		}, nil
	})
}

func mustNames(data []byte) []string {
	_, names, _ := parseEnv(data)
	return names
}

// EnvStatus reports where an app's variables come from. Names only: values are never returned.
type EnvStatus struct {
	EnvFile string `json:"env_file,omitempty"`
	// Pending: the stored environment is not what the running instances have; the next rollout applies it.
	Pending   bool     `json:"pending"`
	File      []string `json:"file"`
	FileError string   `json:"file_error,omitempty"`
	Overrides []string `json:"overrides"`
	Config    []string `json:"config"`
	Reserved  []string `json:"reserved"`
}

func (a *App) envStatus() EnvStatus {
	st := EnvStatus{EnvFile: a.cfg.EnvFile, File: []string{}, Overrides: a.overrideNames(), Config: []string{},
		Reserved: []string{"DROVER_APP", "DROVER_INDEX", "DROVER_VERSION", "PORT"}}
	_, names, err := a.readEnvFile()
	if err != nil {
		st.FileError = err.Error()
	}
	st.File = append(st.File, names...)
	for k := range a.cfg.Env {
		st.Config = append(st.Config, k)
	}
	sort.Strings(st.File)
	sort.Strings(st.Config)
	st.Pending = a.envPending()
	return st
}

func (a *App) overrideNames() []string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	names := make([]string, 0, len(a.overrides))
	for k := range a.overrides {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}
