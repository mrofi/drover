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

	healthy  atomic.Bool
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

func newProc(app *App, slot, port int, version string, argv []string) *proc {
	p := &proc{app: app, slot: slot, port: port, version: version, argv: argv,
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
	cmd.Dir = cfg.Dir
	for _, kv := range os.Environ() {
		// The admin token lets its holder replace binaries: the apps do not get it.
		if !strings.HasPrefix(kv, "DROVER_TOKEN=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, "PORT="+strconv.Itoa(p.port), "DROVER_APP="+cfg.Name,
		"DROVER_INDEX="+strconv.Itoa(p.slot), "DROVER_VERSION="+p.version)
	for k, v := range cfg.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
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
func (p *proc) healthLoop() {
	for {
		ok := p.check()
		select {
		case <-p.stop:
			p.healthy.Store(false)
			return
		default:
		}
		p.healthy.Store(ok)
		wait := p.app.cfg.Health.Interval
		if !ok && wait > 250*time.Millisecond {
			wait = 250 * time.Millisecond
		}
		select {
		case <-p.stop:
			p.healthy.Store(false)
			return
		case <-time.After(wait):
		}
	}
}

func (p *proc) check() bool {
	if p.pid.Load() == 0 {
		return false
	}
	cfg := p.app.cfg.Health
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(p.port))
	if cfg.Path == "" {
		conn, err := net.DialTimeout("tcp", addr, cfg.Timeout)
		if err != nil {
			return false
		}
		conn.Close()
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+cfg.Path, nil)
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
