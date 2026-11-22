package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
)

// Apps can be added and removed through the admin API when admin.manage_apps is on. Their definitions are
// kept in <data>/apps.json, so they come back when drover restarts (as long as the data directory does).
// Apps from the config file are never changed through the API.

const (
	originConfig = "config"
	originAPI    = "api"
)

var (
	errManageOff = errors.New("adding and removing apps through the API is off: set admin.manage_apps (or DROVER_MANAGE_APPS=1, or --manage-apps)")
	errAppExists = errors.New("an app with that name already exists")
)

// appSpec is an app as the API takes it and apps.json keeps it: the keys of an app in the config file,
// with durations as strings ("30s").
type appSpec struct {
	Name         string            `json:"name"`
	Command      Command           `json:"command"`
	Binary       string            `json:"binary,omitempty"`
	Folder       string            `json:"folder,omitempty"`
	Listen       string            `json:"listen"`
	Instances    int               `json:"instances,omitempty"`
	PortBase     int               `json:"port_base,omitempty"`
	Health       healthSpec        `json:"health,omitempty"`
	Ready        healthSpec        `json:"ready,omitempty"`
	StartTimeout string            `json:"start_timeout,omitempty"`
	DrainTimeout string            `json:"drain_timeout,omitempty"`
	StopTimeout  string            `json:"stop_timeout,omitempty"`
	TryDuration  string            `json:"try_duration,omitempty"`
	StopSignal   string            `json:"stop_signal,omitempty"`
	Dir          string            `json:"dir,omitempty"`
	Env          map[string]string `json:"env,omitempty"`
	EnvFile      string            `json:"env_file,omitempty"`
	EnvPrefix    string            `json:"env_prefix,omitempty"`
}

type healthSpec struct {
	Path     string `json:"path,omitempty"`
	Interval string `json:"interval,omitempty"`
	Timeout  string `json:"timeout,omitempty"`
}

// UnmarshalJSON accepts a command as a string (split like the config file does) or as a list of words.
func (c *Command) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		words, err := splitWords(s)
		if err != nil {
			return err
		}
		*c = words
		return nil
	}
	var words []string
	if err := json.Unmarshal(b, &words); err != nil {
		return errors.New("command must be a string or a list of strings")
	}
	*c = words
	return nil
}

// toConfig turns a spec into an app configuration, not yet normalized.
func (s appSpec) toConfig() (*AppConfig, error) {
	cfg := &AppConfig{Name: s.Name, Command: append(Command(nil), s.Command...), Binary: s.Binary,
		Folder: s.Folder, Listen: s.Listen, Instances: s.Instances, PortBase: s.PortBase,
		Health: HealthConfig{Path: s.Health.Path}, Ready: ReadyConfig{Path: s.Ready.Path}, StopSignal: s.StopSignal, Dir: s.Dir, EnvFile: s.EnvFile,
		EnvPrefix: s.EnvPrefix}
	if len(s.Env) > 0 {
		cfg.Env = map[string]string{}
		for k, v := range s.Env {
			if err := checkEnvKey(k); err != nil {
				return nil, fmt.Errorf("env: %w", err)
			}
			cfg.Env[k] = v
		}
	}
	for _, d := range []struct {
		name string
		in   string
		out  *time.Duration
	}{
		{"health.interval", s.Health.Interval, &cfg.Health.Interval},
		{"health.timeout", s.Health.Timeout, &cfg.Health.Timeout},
		{"ready.interval", s.Ready.Interval, &cfg.Ready.Interval},
		{"ready.timeout", s.Ready.Timeout, &cfg.Ready.Timeout},
		{"start_timeout", s.StartTimeout, &cfg.StartTimeout},
		{"drain_timeout", s.DrainTimeout, &cfg.DrainTimeout},
		{"stop_timeout", s.StopTimeout, &cfg.StopTimeout},
		{"try_duration", s.TryDuration, &cfg.TryDuration},
	} {
		if d.in == "" {
			continue
		}
		v, err := time.ParseDuration(d.in)
		if err != nil || v <= 0 {
			return nil, fmt.Errorf("%s: %q is not a duration like 30s", d.name, d.in)
		}
		*d.out = v
	}
	return cfg, nil
}

func overlaps(a, b *AppConfig) bool {
	return a.PortBase < b.PortBase+2*b.Instances && b.PortBase < a.PortBase+2*a.Instances
}

// freePortBase finds the lowest instance port range, from 9000 and below 32768, that no app uses.
func freePortBase(instances int, apps []*App) (int, error) {
	want := &AppConfig{PortBase: 9000, Instances: instances}
	for moved := true; moved; {
		moved = false
		for _, a := range apps {
			if overlaps(want, a.cfg) {
				want.PortBase, moved = a.cfg.PortBase+2*a.cfg.Instances, true
			}
		}
	}
	if want.PortBase+2*instances > 32768 {
		return 0, errors.New("no free instance ports below 32768: set port_base")
	}
	return want.PortBase, nil
}

// prepare checks a spec against the apps there are and returns its configuration, normalized, with a
// port_base picked when it had none. The spec is updated with that port_base, so it stays the same after a
// restart.
func prepare(spec *appSpec, apps []*App) (*AppConfig, error) {
	if !nameRe.MatchString(spec.Name) {
		return nil, fmt.Errorf("invalid name %q", spec.Name)
	}
	for _, a := range apps {
		if a.cfg.Name == spec.Name {
			return nil, errAppExists
		}
	}
	cfg, err := spec.toConfig()
	if err != nil {
		return nil, err
	}
	if cfg.Instances <= 0 {
		cfg.Instances = 1
	}
	if cfg.PortBase == 0 {
		if cfg.PortBase, err = freePortBase(cfg.Instances, apps); err != nil {
			return nil, err
		}
	}
	if err := cfg.normalize(0); err != nil {
		return nil, err
	}
	for _, a := range apps {
		if overlaps(cfg, a.cfg) {
			return nil, fmt.Errorf("instance ports %d-%d overlap those of app %q", cfg.PortBase, cfg.PortBase+2*cfg.Instances-1, a.cfg.Name)
		}
		if prefixesOverlap(cfg.EnvPrefix, a.cfg.EnvPrefix) {
			return nil, fmt.Errorf("env_prefix %q overlaps %q of app %q: one starts with the other", cfg.EnvPrefix, a.cfg.EnvPrefix, a.cfg.Name)
		}
	}
	spec.PortBase, spec.Instances = cfg.PortBase, cfg.Instances
	return cfg, nil
}

func (d *Drover) appsFile() string { return filepath.Join(d.cfg.Data, "apps.json") }

type appsFile struct {
	Apps []appSpec `json:"apps"`
}

// loadManaged reads apps.json and adds the apps it can: one whose name is taken by the config file, or
// that is no longer valid, is left out with a warning but stays in the file.
func (d *Drover) loadManaged() error {
	raw, err := os.ReadFile(d.appsFile())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var f appsFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return fmt.Errorf("%s: %w", d.appsFile(), err)
	}
	for _, spec := range f.Apps {
		d.specs[spec.Name] = spec
		cfg, err := prepare(&spec, d.apps)
		if errors.Is(err, errAppExists) {
			log.Printf("[drover] app %q from the API is not started: the config file defines an app with that name", spec.Name)
			continue
		}
		if err != nil {
			log.Printf("[drover] app %q from the API is not started: %v", spec.Name, err)
			continue
		}
		a, err := newApp(cfg, d.cfg.Data, d.cfg.Cache, d.cfg.LogLines)
		if err != nil {
			log.Printf("[drover] app %q from the API is not started: %v", spec.Name, err)
			continue
		}
		a.origin = originAPI
		a.envPrefixes = d.envPrefixes
		d.apps = append(d.apps, a)
	}
	return nil
}

// saveManaged writes the specs of the API's apps. The file can hold env values, so only drover may read it.
func (d *Drover) saveManaged() error {
	f := appsFile{Apps: []appSpec{}}
	for _, s := range d.specs {
		f.Apps = append(f.Apps, s)
	}
	sort.Slice(f.Apps, func(i, j int) bool { return f.Apps[i].Name < f.Apps[j].Name })
	raw, _ := json.MarshalIndent(f, "", "  ")
	if err := os.MkdirAll(d.cfg.Data, 0o755); err != nil {
		return err
	}
	return writeFileAtomic(d.appsFile(), append(raw, '\n'), 0o600)
}

// addApp creates, starts and records an app. It returns once the app's port is open; its instances start
// in the background, or wait for a first release when there is nothing to run yet.
func (d *Drover) addApp(spec appSpec) (*App, error) {
	d.manageMu.Lock()
	defer d.manageMu.Unlock()
	if _, taken := d.specs[spec.Name]; taken {
		// Kept in the file but not running (its name is in the config file, or it is broken).
		return nil, fmt.Errorf("%w (it is recorded but not running; remove it first)", errAppExists)
	}
	cfg, err := prepare(&spec, d.appList())
	if err != nil {
		return nil, err
	}
	dataDir := filepath.Join(d.cfg.Data, cfg.Name)
	_, statErr := os.Stat(dataDir)
	a, err := newApp(cfg, d.cfg.Data, d.cfg.Cache, d.cfg.LogLines)
	if err != nil {
		return nil, err
	}
	a.origin = originAPI
	a.envPrefixes = d.envPrefixes
	undo := func() {
		if errors.Is(statErr, os.ErrNotExist) {
			os.RemoveAll(dataDir)
		}
	}
	if err := d.listenApp(a); err != nil {
		undo()
		return nil, err
	}
	d.specs[spec.Name] = spec
	if err := d.saveManaged(); err != nil {
		delete(d.specs, spec.Name)
		d.closeApp(a)
		undo()
		return nil, fmt.Errorf("recording the app: %w", err)
	}
	d.appsMu.Lock()
	d.apps = append(d.apps, a)
	d.appsMu.Unlock()
	a.start()
	a.logf("added through the API")
	return a, nil
}

// removeApp stops an app added through the API and forgets it. With purge, its versions go too.
func (d *Drover) removeApp(name string, purge bool) error {
	d.manageMu.Lock()
	defer d.manageMu.Unlock()
	a := d.findApp(name)
	if _, recorded := d.specs[name]; !recorded {
		if a != nil {
			return errors.New("this app is defined in the config file: remove it there and restart drover")
		}
		return errNotFound
	}
	if a != nil {
		if !a.rollMu.TryLock() {
			return errBusy
		}
		defer a.rollMu.Unlock()
	}
	spec := d.specs[name]
	delete(d.specs, name)
	if err := d.saveManaged(); err != nil {
		d.specs[name] = spec
		return fmt.Errorf("recording the change: %w", err)
	}
	if a != nil {
		d.appsMu.Lock()
		for i, x := range d.apps {
			if x == a {
				d.apps = append(d.apps[:i:i], d.apps[i+1:]...)
				break
			}
		}
		d.appsMu.Unlock()
		d.closeApp(a)
		log.Printf("[%s] removed through the API", name)
	}
	if purge {
		_ = os.RemoveAll(filepath.Join(d.cfg.Cache, name))
		if err := os.RemoveAll(filepath.Join(d.cfg.Data, name)); err != nil {
			return fmt.Errorf("the app is removed, but its versions could not be deleted: %w", err)
		}
	}
	return nil
}

// closeApp closes an app's port, giving open requests a moment, then stops its instances.
func (d *Drover) closeApp(a *App) {
	if a.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = a.server.Shutdown(ctx)
		cancel()
	}
	a.stop()
}

func (d *Drover) manageRoutes(g *echo.Group) {
	g.POST("/apps", func(c echo.Context) error {
		if !d.cfg.Admin.ManageApps {
			return echo.NewHTTPError(http.StatusForbidden, errManageOff.Error())
		}
		var spec appSpec
		dec := json.NewDecoder(c.Request().Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&spec); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "the body must be a JSON app: "+err.Error())
		}
		a, err := d.addApp(spec)
		switch {
		case errors.Is(err, errAppExists):
			return echo.NewHTTPError(http.StatusConflict, err.Error())
		case err != nil && (strings.Contains(err.Error(), "overlap") || strings.Contains(err.Error(), "address already in use")):
			return echo.NewHTTPError(http.StatusConflict, err.Error())
		case err != nil && strings.HasPrefix(err.Error(), "recording"):
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		case err != nil:
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		return c.JSON(http.StatusCreated, a.status())
	})
	g.DELETE("/apps/:app", func(c echo.Context) error {
		if !d.cfg.Admin.ManageApps {
			return echo.NewHTTPError(http.StatusForbidden, errManageOff.Error())
		}
		err := d.removeApp(c.Param("app"), c.QueryParam("purge") == "true")
		switch {
		case err == nil:
			return c.NoContent(http.StatusNoContent)
		case errors.Is(err, errNotFound):
			return echo.NewHTTPError(http.StatusNotFound, "no such app")
		case errors.Is(err, errBusy):
			return echo.NewHTTPError(http.StatusConflict, "a rollout of this app is running: try again when it is over")
		case strings.Contains(err.Error(), "config file"):
			return echo.NewHTTPError(http.StatusConflict, err.Error())
		}
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	})
	// The definition an app was added with, for apps from the API: what apps.json holds for it.
	g.GET("/apps/:app/spec", func(c echo.Context) error {
		d.manageMu.Lock()
		spec, ok := d.specs[c.Param("app")]
		d.manageMu.Unlock()
		if !ok {
			return echo.NewHTTPError(http.StatusNotFound, "no app with that name was added through the API")
		}
		// Variable names only, like everywhere else in the API.
		names := []string{}
		for k := range spec.Env {
			names = append(names, k)
		}
		sort.Strings(names)
		spec.Env = nil
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetIndent("", "  ")
		_ = enc.Encode(struct {
			appSpec
			EnvNames []string `json:"env_names"`
		}{spec, names})
		return c.Blob(http.StatusOK, echo.MIMEApplicationJSON, buf.Bytes())
	})
}
