package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// proc is one supervised instance of an app: a command on its own loopback port. It is restarted when it
// exits until shutdown is called. A rollout never changes a proc; it starts a new one and retires the old.
type proc struct {
	app     *App
	slot    int
	port    int
	version string
	argv    []string
	env     []string          // config, env_file and overrides, as they were when the instance was created
	repl    *strings.Replacer // {dir}, {bin}, {port}, {index} for this instance
	workdir string
	release string // the folder of its version, for a folder app

	healthy  atomic.Bool
	readyAt  atomic.Int64 // the start (started) at which the instance passed ready.path
	draining atomic.Bool
	inflight atomic.Int64
	exits    atomic.Int64
	pid      atomic.Int64
	started  atomic.Int64 // unix nanoseconds of the last start

	proxy    *httputil.ReverseProxy
	tailMu   sync.Mutex
	tail     []string // the last lines it printed
	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
}

type proxyErrKey struct{}

func newProc(app *App, slot, port int, version string, argv, env []string) *proc {
	p := &proc{app: app, slot: slot, port: port, version: version, argv: argv, env: env,
		stop: make(chan struct{}), done: make(chan struct{})}
	target := &url.URL{Scheme: "http", Host: net.JoinHostPort("127.0.0.1", strconv.Itoa(port))}
	p.proxy = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = pr.In.Host
			pr.Out.Header["X-Forwarded-For"] = pr.In.Header["X-Forwarded-For"]
			pr.SetXForwarded()
		},
		Transport:     app.transport,
		FlushInterval: -1, // stream: SSE and chunked responses are passed on as they arrive
		// The error is handed back to the load balancer, which decides between a retry and a 502.
		ErrorHandler: func(_ http.ResponseWriter, r *http.Request, err error) {
			if dst, ok := r.Context().Value(proxyErrKey{}).(*error); ok {
				*dst = err
			}
		},
		ErrorLog: log.New(discard{}, "", 0),
	}
	return p
}

type discard struct{}

func (discard) Write(b []byte) (int, error) { return len(b), nil }

func (p *proc) label() string { return fmt.Sprintf("%s#%d", p.app.cfg.Name, p.slot) }

// run starts the command and restarts it, with a growing delay, every time it exits.
func (p *proc) run() {
	defer close(p.done)
	backoff := time.Second
	for {
		started := time.Now()
		p.started.Store(started.UnixNano())
		cmd := p.command()
		err := cmd.Start()
		if err == nil {
			p.pid.Store(int64(cmd.Process.Pid))
			waited := make(chan error, 1)
			go func() { waited <- cmd.Wait() }()
			select {
			case err = <-waited:
			case <-p.stop:
				p.terminate(cmd, waited)
				p.pid.Store(0)
				p.healthy.Store(false)
				return
			}
		}
		p.pid.Store(0)
		p.healthy.Store(false)
		p.exits.Add(1)
		p.app.metrics.exits.Add(1)
		select {
		case <-p.stop:
			return
		default:
		}
		if time.Since(started) > 10*time.Second {
			backoff = time.Second
		}
		p.app.logf("instance %d exited (%v), restarting in %s", p.slot, err, backoff)
		select {
		case <-p.stop:
			return
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
	}
}

func (p *proc) command() *exec.Cmd {
	cfg := p.app.cfg
	cmd := exec.Command(p.argv[0], p.argv[1:]...)
	cmd.Dir = p.workdir
	var others []string
	if p.app.envPrefixes != nil {
		others = p.app.envPrefixes()
	}
	cmd.Env = baseEnv(os.Environ(), cfg.EnvPrefix, others)
	for _, kv := range p.env {
		cmd.Env = append(cmd.Env, p.repl.Replace(kv)) // PYTHONPATH={dir}/deps and the like
	}
	if p.release != "" {
		cmd.Env = append(cmd.Env, "DROVER_DIR="+p.release)
	}
	// Last, so nothing above can give two instances the same port.
	cmd.Env = append(cmd.Env, "PORT="+strconv.Itoa(p.port), "DROVER_APP="+cfg.Name,
		"DROVER_INDEX="+strconv.Itoa(p.slot), "DROVER_VERSION="+p.version)
	out := &prefixWriter{prefix: "[" + p.label() + "] ", p: p}
	cmd.Stdout, cmd.Stderr = out, out
	// Its own process group, so stopping it also stops anything it started.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// A grandchild holding the output pipe open must not keep Wait from returning.
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

// terminate sends the stop signal to the process group, then SIGKILL if it is still there after stop_timeout.
func (p *proc) terminate(cmd *exec.Cmd, waited <-chan error) {
	pid := cmd.Process.Pid
	_ = syscall.Kill(-pid, p.app.cfg.stopSignal)
	select {
	case <-waited:
		return
	case <-time.After(p.app.cfg.StopTimeout):
	}
	p.app.logf("instance %d still running after %s, killing", p.slot, p.app.cfg.StopTimeout)
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	<-waited
}

// shutdown stops the instance for good and waits until it is gone.
func (p *proc) shutdown() {
	p.stopOnce.Do(func() { close(p.stop) })
	<-p.done
}

// healthLoop keeps p.healthy current. It checks more often while the instance is not healthy.
//
// With ready.path, a process that started counts as healthy only once it passed that path; from then on
// health.path alone decides. Readiness belongs to one start of the process: a restarted one has to pass again.
func (p *proc) healthLoop() {
	for {
		var ok bool
		wait := p.app.cfg.Health.Interval
		if ready := p.app.cfg.Ready; ready.Path != "" && !p.isReady() {
			wait = ready.Interval
			started := p.started.Load()
			if ok = p.check(ready.Path, ready.Timeout); ok {
				p.readyAt.Store(started) // a stale start never equals the current one, so this cannot leak over a restart
				ok = p.isReady()
			}
		} else {
			ok = p.check(p.app.cfg.Health.Path, p.app.cfg.Health.Timeout)
			if !ok && wait > 250*time.Millisecond {
				wait = 250 * time.Millisecond
			}
		}
		select {
		case <-p.stop:
			p.healthy.Store(false)
			return
		default:
		}
		p.healthy.Store(ok)
		if ok && wait < p.app.cfg.Health.Interval {
			wait = p.app.cfg.Health.Interval // just became ready: from now on the health check's pace
		}
		select {
		case <-p.stop:
			p.healthy.Store(false)
			return
		case <-time.After(wait):
		}
	}
}

// isReady reports whether the running process passed ready.path (always true without one).
func (p *proc) isReady() bool {
	if p.app.cfg.Ready.Path == "" {
		return true
	}
	s := p.started.Load()
	return s != 0 && p.readyAt.Load() == s
}

// check asks the instance for path (GET, 2xx/3xx); an empty path only checks that the port takes a connection.
func (p *proc) check(path string, timeout time.Duration) bool {
	if p.pid.Load() == 0 {
		return false
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(p.port))
	if path == "" {
		conn, err := net.DialTimeout("tcp", addr, timeout)
		if err != nil {
			return false
		}
		conn.Close()
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+path, nil)
	if err != nil {
		return false
	}
	resp, err := p.app.healthClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 400
}

// waitHealthy returns nil once the instance passes a health check, or an error when it keeps exiting or
// does not get healthy in time.
func (p *proc) waitHealthy(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if p.healthy.Load() {
			return nil
		}
		if p.exits.Load() >= 2 {
			return errors.New("the process keeps exiting right after start")
		}
		if time.Now().After(deadline) {
			if !p.isReady() {
				return fmt.Errorf("not ready after %s (%s did not answer 2xx or 3xx)", timeout, p.app.cfg.Ready.Path)
			}
			return fmt.Errorf("not healthy after %s", timeout)
		}
		select {
		case <-p.stop:
			return errors.New("stopped while starting")
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (p *proc) state() string {
	switch {
	case p.draining.Load():
		return "draining"
	case p.healthy.Load():
		return "healthy"
	case p.pid.Load() != 0 && !p.isReady():
		return "starting"
	case p.pid.Load() == 0:
		return "down"
	default:
		return "unhealthy"
	}
}

// keep records one line of output: in the app's log buffer and in the instance's own short tail.
func (p *proc) keep(line string) {
	p.app.logs.add(p.slot, line)
	p.tailMu.Lock()
	if p.tail = append(p.tail, line); len(p.tail) > 10 {
		p.tail = p.tail[1:]
	}
	p.tailMu.Unlock()
}

// lastOutput returns the last few lines the instance printed, indented, for error messages.
func (p *proc) lastOutput() string {
	p.tailMu.Lock()
	defer p.tailMu.Unlock()
	if len(p.tail) == 0 {
		return ""
	}
	return "  " + strings.Join(p.tail, "\n  ")
}

var outMu sync.Mutex

// prefixWriter writes complete lines to drover's stdout, each with the instance's label in front, and
// keeps a copy in the app's log buffer.
type prefixWriter struct {
	prefix string
	p      *proc
	buf    []byte
}

func (w *prefixWriter) Write(b []byte) (int, error) {
	w.buf = append(w.buf, b...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			if len(w.buf) < 64<<10 {
				break
			}
			w.buf = append(w.buf, '\n')
			i = len(w.buf) - 1
		}
		outMu.Lock()
		os.Stdout.WriteString(w.prefix)
		os.Stdout.Write(w.buf[:i+1])
		outMu.Unlock()
		w.p.keep(string(bytes.TrimRight(w.buf[:i], "\r")))
		w.buf = w.buf[i+1:]
	}
	return len(b), nil
}
