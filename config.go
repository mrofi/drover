package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the whole drover configuration: one admin API and one or more apps.
type Config struct {
	// Data is where releases and the current/previous version of each app are kept.
	Data  string      `yaml:"data"`
	Admin AdminConfig `yaml:"admin"`
	// ImageFetchURL is the base URL of an image-file-fetch server, used for {"image": ..., "path": ...} sources.
	ImageFetchURL string `yaml:"image_fetch_url"`
	// MaxBinaryBytes caps the size of one release (default 1 GiB).
	MaxBinaryBytes int64 `yaml:"max_binary_bytes"`
	// LogLines is how many recent lines of each app are kept in memory for the logs endpoint (default 1000).
	LogLines int           `yaml:"log_lines"`
	Metrics  MetricsConfig `yaml:"metrics"`
	Apps     []*AppConfig  `yaml:"apps"`
}

// MetricsConfig opens a second listener that serves only GET /metrics, without a token, so Prometheus can
// scrape it from outside the container. /metrics is always on the admin API as well.
type MetricsConfig struct {
	Listen string `yaml:"listen"`
}

type AdminConfig struct {
	Listen string `yaml:"listen"`
	Token  string `yaml:"token"`
	// ManageApps lets the API add and remove apps. Off by default: an app's command can be anything.
	ManageApps bool `yaml:"manage_apps"`
}

type AppConfig struct {
	Name string `yaml:"name"`
	// Command is the argv of one instance. {bin}, {port} and {index} are replaced per instance.
	Command Command `yaml:"command"`
	// Binary is the file a release replaces. Default: the first word of Command.
	Binary string `yaml:"binary"`
	// Folder makes this a folder app: a release replaces a whole folder, {dir} in the command (and in dir
	// and env values) is the folder of the version an instance runs, and it is the working directory.
	// Folder itself is the built-in version; it may not exist yet.
	Folder string `yaml:"folder"`
	// Listen is the public address the load balancer serves on.
	Listen    string `yaml:"listen"`
	Instances int    `yaml:"instances"`
	// PortBase is the first loopback port given to instances. An app uses PortBase .. PortBase+2*Instances-1.
	PortBase     int               `yaml:"port_base"`
	Health       HealthConfig      `yaml:"health"`
	StartTimeout time.Duration     `yaml:"start_timeout"`
	DrainTimeout time.Duration     `yaml:"drain_timeout"`
	StopTimeout  time.Duration     `yaml:"stop_timeout"`
	TryDuration  time.Duration     `yaml:"try_duration"`
	StopSignal   string            `yaml:"stop_signal"`
	Env          map[string]string `yaml:"env"`
	// EnvFile is a KEY=VALUE file read every time an instance is started. It wins over Env.
	EnvFile string `yaml:"env_file"`
	Dir     string `yaml:"dir"`

	stopSignal syscall.Signal
	folderMode bool
}

type HealthConfig struct {
	// Path is requested with GET; 2xx and 3xx mean healthy. Empty: a TCP connect to the port is the check.
	Path     string        `yaml:"path"`
	Interval time.Duration `yaml:"interval"`
	Timeout  time.Duration `yaml:"timeout"`
}

// Command accepts either a YAML list (used as is) or a string (split like a shell would, without running one).
type Command []string

func (c *Command) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		words, err := splitWords(n.Value)
		if err != nil {
			return err
		}
		*c = words
		return nil
	}
	var words []string
	if err := n.Decode(&words); err != nil {
		return err
	}
	*c = words
	return nil
}

// splitWords splits a command line into words, honouring single quotes, double quotes and backslashes.
func splitWords(s string) ([]string, error) {
	var words []string
	var cur strings.Builder
	inWord := false
	quote := rune(0)
	escaped := false
	for _, r := range s {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
		case r == '\\' && quote != '\'':
			escaped = true
			inWord = true
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote = r
			inWord = true
		case r == ' ' || r == '\t' || r == '\n':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if quote != 0 || escaped {
		return nil, errors.New("unterminated quote or escape in command")
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words, nil
}

var signals = map[string]syscall.Signal{
	"TERM": syscall.SIGTERM, "INT": syscall.SIGINT, "QUIT": syscall.SIGQUIT, "HUP": syscall.SIGHUP,
	"KILL": syscall.SIGKILL, "USR1": syscall.SIGUSR1, "USR2": syscall.SIGUSR2,
}

var nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

func loadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg := &Config{}
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// normalize fills in defaults and rejects configurations that cannot work.
func (c *Config) normalize() error {
	if v := os.Getenv("DROVER_DATA"); v != "" && c.Data == "" {
		c.Data = v
	}
	if c.Data == "" {
		if home, err := os.UserHomeDir(); err == nil && home != "" && home != "/" {
			c.Data = filepath.Join(home, ".local", "share", "drover")
		} else {
			c.Data = filepath.Join(os.TempDir(), "drover")
		}
	}
	if v := os.Getenv("DROVER_TOKEN"); v != "" {
		c.Admin.Token = v
	}
	if v := os.Getenv("DROVER_IMAGE_FETCH_URL"); v != "" {
		c.ImageFetchURL = v
	}
	c.ImageFetchURL = strings.TrimRight(c.ImageFetchURL, "/")
	if c.Admin.Listen == "" {
		c.Admin.Listen = "127.0.0.1:9100"
	}
	// The admin API can replace the binaries drover runs, so it is never open to the network without a token.
	if c.Admin.Token == "" && !isLoopback(c.Admin.Listen) {
		return fmt.Errorf("admin.listen %q is not loopback: set admin.token or DROVER_TOKEN", c.Admin.Listen)
	}
	if c.LogLines <= 0 {
		c.LogLines = 1000
	}
	if c.MaxBinaryBytes <= 0 {
		c.MaxBinaryBytes = 1 << 30
	}
	if v := os.Getenv("DROVER_MANAGE_APPS"); v == "1" || strings.EqualFold(v, "true") {
		c.Admin.ManageApps = true
	}
	if len(c.Apps) == 0 && !c.Admin.ManageApps {
		return errors.New("no apps configured (or turn on admin.manage_apps to add them through the API)")
	}
	seen := map[string]bool{}
	for i, a := range c.Apps {
		if !nameRe.MatchString(a.Name) {
			return fmt.Errorf("app %d: invalid name %q", i, a.Name)
		}
		if seen[a.Name] {
			return fmt.Errorf("app %q is defined twice", a.Name)
		}
		seen[a.Name] = true
		if err := a.normalize(i); err != nil {
			return fmt.Errorf("app %q: %w", a.Name, err)
		}
	}
	for i, a := range c.Apps {
		for _, b := range c.Apps[i+1:] {
			if overlaps(a, b) {
				return fmt.Errorf("apps %q and %q have overlapping instance ports (set port_base)", a.Name, b.Name)
			}
		}
	}
	return nil
}

func (a *AppConfig) normalize(index int) error {
	if len(a.Command) == 0 {
		return errors.New("command is required")
	}
	if a.Listen == "" {
		return errors.New("listen is required")
	}
	if a.Instances <= 0 {
		a.Instances = 1
	}
	if a.PortBase == 0 {
		a.PortBase = 9000 + 200*index
	}
	if a.PortBase < 1 || a.PortBase+2*a.Instances > 65536 {
		return fmt.Errorf("port_base %d with %d instances is out of range", a.PortBase, a.Instances)
	}
	// The first word of the command is the updatable binary unless the command says where {bin} goes.
	// A folder app instead runs from a folder that versions replace as a whole: {dir}.
	hasBin, hasDir := false, strings.Contains(a.Dir, "{dir}")
	for _, w := range a.Command {
		hasBin = hasBin || strings.Contains(w, "{bin}")
		hasDir = hasDir || strings.Contains(w, "{dir}")
	}
	a.folderMode = hasDir || a.Folder != ""
	if a.folderMode {
		if hasBin || a.Binary != "" {
			return errors.New("a folder app (folder: or {dir}) cannot also use binary: or {bin}")
		}
		if a.Folder != "" {
			if abs, err := filepath.Abs(a.Folder); err == nil {
				a.Folder = abs
			}
		}
	} else if !hasBin {
		if a.Binary == "" {
			a.Binary = a.Command[0]
		}
		if a.Binary == a.Command[0] {
			a.Command = append(Command{"{bin}"}, a.Command[1:]...)
		} else {
			return errors.New("binary is set but command has no {bin} placeholder")
		}
	}
	if a.EnvFile != "" {
		if abs, err := filepath.Abs(a.EnvFile); err == nil {
			a.EnvFile = abs
		}
	}
	if a.Binary != "" {
		if p, err := exec.LookPath(a.Binary); err == nil {
			a.Binary = p
		}
		if abs, err := filepath.Abs(a.Binary); err == nil {
			a.Binary = abs
		}
	}
	setDefault(&a.Health.Interval, 2*time.Second)
	setDefault(&a.Health.Timeout, 2*time.Second)
	setDefault(&a.StartTimeout, 30*time.Second)
	setDefault(&a.DrainTimeout, 30*time.Second)
	setDefault(&a.StopTimeout, 10*time.Second)
	setDefault(&a.TryDuration, 5*time.Second)
	if a.Health.Path != "" && !strings.HasPrefix(a.Health.Path, "/") {
		a.Health.Path = "/" + a.Health.Path
	}
	name := strings.TrimPrefix(strings.ToUpper(a.StopSignal), "SIG")
	if name == "" {
		name = "TERM"
	}
	sig, ok := signals[name]
	if !ok {
		return fmt.Errorf("unknown stop_signal %q", a.StopSignal)
	}
	a.stopSignal = sig
	return nil
}

func setDefault(d *time.Duration, v time.Duration) {
	if *d <= 0 {
		*d = v
	}
}

func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
