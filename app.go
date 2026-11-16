package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var errBusy = errors.New("another rollout of this app is in progress")

// App supervises the instances of one app and load balances its public port across them.
type App struct {
	cfg          *AppConfig
	store        *releaseStore
	transport    *http.Transport
	healthClient *http.Client
	logs         *logBuffer
	metrics      *appMetrics

	mu        sync.RWMutex
	slots     []*proc        // the instance serving each slot; nil while a slot has nothing to run
	procs     map[*proc]bool // every live proc, including ones being started or drained
	ports     map[int]bool   // instance ports in use
	version   string         // version the slots run (or are being rolled back to)
	previous  string
	closed    bool
	overrides map[string]string // variables set through the API, in memory only

	rollMu  sync.Mutex
	rolling atomic.Bool
	next    atomic.Uint64
	addr    net.Addr
}

func newApp(cfg *AppConfig, dataDir string, logLines int) (*App, error) {
	store, err := newReleaseStore(dataDir, cfg)
	if err != nil {
		return nil, err
	}
	a := &App{cfg: cfg, store: store, procs: map[*proc]bool{}, ports: map[int]bool{},
		slots: make([]*proc, cfg.Instances)}
	a.logs, a.metrics = newLogBuffer(logLines), newAppMetrics()
	a.transport = &http.Transport{
		DialContext:         (&net.Dialer{Timeout: 2 * time.Second}).DialContext,
		MaxIdleConnsPerHost: 64,
		IdleConnTimeout:     60 * time.Second,
		DisableCompression:  true, // pass bodies through as the app sent them
	}
	a.healthClient = &http.Client{
		Transport:     &http.Transport{DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	st := store.loadState()
	a.version, a.previous = st.Current, st.Previous
	if _, err := store.binPath(a.version); err != nil && a.version != versionBuiltin {
		log.Printf("[%s] release %q is missing, falling back to the built-in binary", cfg.Name, a.version)
		a.version = versionBuiltin
	}
	return a, nil
}

// logf logs an event about the app to drover's output and to the app's log buffer.
func (a *App) logf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	log.Printf("[%s] %s", a.cfg.Name, msg)
	a.logs.add(-1, msg)
}

// start launches every slot on the current version. It does not wait for them to become healthy.
func (a *App) start() {
	if _, err := a.store.binPath(a.version); err != nil {
		a.logf("nothing to run yet (%v): waiting for a first release", err)
		return
	}
	for i := range a.slots {
		p, err := a.spawn(i, a.version)
		if err != nil {
			a.logf("slot %d: %v", i, err)
			continue
		}
		a.mu.Lock()
		a.slots[i] = p
		a.mu.Unlock()
	}
}

// stop shuts down every instance, including ones a rollout was in the middle of starting.
func (a *App) stop() {
	a.mu.Lock()
	a.closed = true
	procs := make([]*proc, 0, len(a.procs))
	for p := range a.procs {
		procs = append(procs, p)
	}
	a.mu.Unlock()
	var wg sync.WaitGroup
	for _, p := range procs {
		wg.Add(1)
		go func() { defer wg.Done(); p.shutdown() }()
	}
	wg.Wait()
}

// spawn starts a new instance of version on a free port. The caller decides when it takes a slot.
func (a *App) spawn(slot int, version string) (*proc, error) {
	bin, err := a.store.binPath(version)
	if err != nil {
		return nil, err
	}
	// Read here, once per instance: a restart after a crash reuses what the instance was started with, so
	// an edited file takes effect only through a rollout, where a bad value can be undone.
	fileVars, _, err := a.readEnvFile()
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return nil, errors.New("drover is shutting down")
	}
	port := 0
	for c := a.cfg.PortBase; c < a.cfg.PortBase+2*a.cfg.Instances; c++ {
		if !a.ports[c] && portFree(c) {
			port = c
			break
		}
	}
	if port == 0 {
		return nil, fmt.Errorf("no free port in %d-%d", a.cfg.PortBase, a.cfg.PortBase+2*a.cfg.Instances-1)
	}
	repl := strings.NewReplacer("{bin}", bin, "{port}", strconv.Itoa(port), "{index}", strconv.Itoa(slot))
	argv := make([]string, len(a.cfg.Command))
	for i, w := range a.cfg.Command {
		argv[i] = repl.Replace(w)
	}
	p := newProc(a, slot, port, version, argv, a.instanceEnv(fileVars))
	a.ports[port] = true
	a.procs[p] = true
	go p.run()
	go p.healthLoop()
	return p, nil
}

func portFree(port int) bool {
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return false
	}
	ln.Close()
	return true
}

// retire stops an instance that no longer has a slot. With drain it first waits for its requests to finish.
func (a *App) retire(p *proc, drain bool) {
	if drain {
		p.draining.Store(true)
		deadline := time.Now().Add(a.cfg.DrainTimeout)
		for p.inflight.Load() > 0 && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		if n := p.inflight.Load(); n > 0 {
			a.logf("instance %d: %d request(s) still open after %s, stopping anyway", p.slot, n, a.cfg.DrainTimeout)
		}
	}
	p.shutdown()
	a.mu.Lock()
	delete(a.procs, p)
	delete(a.ports, p.port)
	a.mu.Unlock()
}

// replace swaps one slot to version without a gap: the new instance is started beside the old one and
// takes the slot only once it is healthy; then the old one is drained and stopped. If the new instance
// does not get healthy, the old one is left serving and an error is returned.
func (a *App) replace(slot int, version string) error {
	np, err := a.spawn(slot, version)
	if err != nil {
		return err
	}
	if err := np.waitHealthy(a.cfg.StartTimeout); err != nil {
		// Say why: the caller of a failed rollout cannot see the instance's output otherwise.
		if out := np.lastOutput(); out != "" {
			err = fmt.Errorf("%w; its last output:\n%s", err, out)
		}
		a.retire(np, false)
		return err
	}
	a.mu.Lock()
	old := a.slots[slot]
	a.slots[slot] = np
	a.mu.Unlock()
	if old != nil {
		a.retire(old, true)
	}
	return nil
}

// rollout moves every slot to target, one at a time. On a failure the slots already moved are put back on
// the version they had, and the current version is left unchanged. target may equal the current version,
// which makes it a rolling restart.
func (a *App) rollout(target string) error {
	if !a.rollMu.TryLock() {
		return errBusy
	}
	defer a.rollMu.Unlock()
	return a.rolloutLocked(target, true)
}

// rolloutLocked is rollout for callers that already hold rollMu. With envHint, a failure while an
// environment change was waiting to be applied says so: the cause may be a variable, not the binary.
func (a *App) rolloutLocked(target string, envHint bool) error {
	envHint = envHint && a.envPending()
	a.rolling.Store(true)
	defer a.rolling.Store(false)

	if _, err := a.store.binPath(target); err != nil {
		return err
	}
	a.mu.RLock()
	old := a.version
	a.mu.RUnlock()
	a.logf("rollout %s -> %s", old, target)
	for i := range a.slots {
		err := a.replace(i, target)
		if err == nil {
			continue
		}
		a.logf("rollout to %s failed at instance %d: %v; putting %d instance(s) back on %s", target, i, err, i, old)
		for j := 0; j < i; j++ {
			if rerr := a.replace(j, old); rerr != nil {
				a.logf("instance %d could not go back to %s: %v", j, old, rerr)
			}
		}
		a.metrics.rolloutsFail.Add(1)
		err = fmt.Errorf("instance %d on version %s: %w", i, target, err)
		if envHint {
			err = fmt.Errorf("%w\nnote: the app's environment was changed since the running instances started and this rollout was the first to use it, so the cause may be a variable and not the binary (see `drover env %s`)", err, a.cfg.Name)
		}
		return err
	}
	a.mu.Lock()
	if target != old {
		a.version, a.previous = target, old
	}
	st := releaseState{Current: a.version, Previous: a.previous}
	a.mu.Unlock()
	if err := a.store.saveState(st); err != nil {
		a.logf("could not save the current version: %v", err)
	}
	a.metrics.rolloutsOK.Add(1)
	a.logf("now on %s", target)
	return nil
}

// forgetPrevious clears the rollback target when its release is deleted.
func (a *App) forgetPrevious(version string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.previous == version {
		a.previous = ""
		_ = a.store.saveState(releaseState{Current: a.version, Previous: ""})
	}
}

func (a *App) versions() (current, previous string) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.version, a.previous
}

// pick returns the healthy instance with the fewest open requests that was not tried yet, and counts the
// request against it. Counting under the same lock a slot swap takes means a drained instance cannot be
// handed a request after its count was read as zero.
func (a *App) pick(tried map[*proc]bool) *proc {
	a.mu.RLock()
	defer a.mu.RUnlock()
	n := len(a.slots)
	offset := int(a.next.Add(1) % uint64(n))
	var best *proc
	for i := 0; i < n; i++ {
		p := a.slots[(offset+i)%n]
		if p == nil || tried[p] || !p.healthy.Load() {
			continue
		}
		if best == nil || p.inflight.Load() < best.inflight.Load() {
			best = p
		}
	}
	if best != nil {
		best.inflight.Add(1)
	}
	return best
}

// ServeHTTP proxies one request. A request whose instance cannot be connected to has not been sent, so it
// is retried on another instance; when none is healthy it waits up to try_duration for one.
func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Body != nil && r.Body != http.NoBody {
		// The transport closes the body of a request it fails to send; keep it open for the retry.
		r.Body = io.NopCloser(r.Body)
	}
	deadline := time.Now().Add(a.cfg.TryDuration)
	tried := map[*proc]bool{}
	for {
		p := a.pick(tried)
		if p == nil {
			if time.Now().After(deadline) || r.Context().Err() != nil {
				http.Error(w, "no healthy instance", http.StatusServiceUnavailable)
				return
			}
			clear(tried)
			select {
			case <-r.Context().Done():
			case <-time.After(100 * time.Millisecond):
			}
			continue
		}
		err := a.forward(p, w, r)
		if err == nil {
			return
		}
		var op *net.OpError
		if errors.As(err, &op) && op.Op == "dial" {
			p.healthy.Store(false)
			a.metrics.retries.Add(1)
			tried[p] = true
			continue
		}
		if r.Context().Err() == nil {
			http.Error(w, "upstream error", http.StatusBadGateway)
		}
		return
	}
}

func (a *App) forward(p *proc, w http.ResponseWriter, r *http.Request) error {
	defer p.inflight.Add(-1)
	var perr error
	p.proxy.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), proxyErrKey{}, &perr)))
	return perr
}

// InstanceStatus and AppStatus are what the admin API reports.
type InstanceStatus struct {
	Slot     int        `json:"slot"`
	Port     int        `json:"port,omitempty"`
	PID      int64      `json:"pid,omitempty"`
	Version  string     `json:"version,omitempty"`
	State    string     `json:"state"`
	Inflight int64      `json:"inflight"`
	Restarts int64      `json:"restarts"`
	Started  *time.Time `json:"started,omitempty"`
}

type AppStatus struct {
	Name     string `json:"name"`
	Listen   string `json:"listen"`
	Version  string `json:"version"`
	Previous string `json:"previous,omitempty"`
	Rolling  bool   `json:"rolling"`
	// EnvPending: the stored environment differs from what the running instances were started with.
	EnvPending bool `json:"env_pending,omitempty"`
	// EnvOverrides are the names of variables set through the API. They are lost when drover restarts.
	EnvOverrides []string         `json:"env_overrides,omitempty"`
	Instances    []InstanceStatus `json:"instances"`
}

func (a *App) status() AppStatus {
	fileVars, _, fileErr := a.readEnvFile()
	a.mu.RLock()
	defer a.mu.RUnlock()
	st := AppStatus{Name: a.cfg.Name, Listen: a.cfg.Listen, Version: a.version, Previous: a.previous,
		Rolling: a.rolling.Load()}
	for k := range a.overrides {
		st.EnvOverrides = append(st.EnvOverrides, k)
	}
	sort.Strings(st.EnvOverrides)
	st.EnvPending = a.envPendingLocked(fileVars, fileErr)
	if a.addr != nil {
		st.Listen = a.addr.String()
	}
	for i, p := range a.slots {
		if p == nil {
			st.Instances = append(st.Instances, InstanceStatus{Slot: i, State: "empty"})
			continue
		}
		started := time.Unix(0, p.started.Load()).UTC()
		st.Instances = append(st.Instances, InstanceStatus{Slot: i, Port: p.port, PID: p.pid.Load(),
			Version: p.version, State: p.state(), Inflight: p.inflight.Load(), Restarts: p.exits.Load(),
			Started: &started})
	}
	return st
}
