package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

var binV1, binV2 string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "drover-test-")
	if err != nil {
		panic(err)
	}
	binV1, binV2 = filepath.Join(dir, "fakeapp-v1"), filepath.Join(dir, "fakeapp-v2")
	for bin, v := range map[string]string{binV1: "v1", binV2: "v2"} {
		out, err := exec.Command("go", "build", "-ldflags", "-X main.version="+v, "-o", bin, "./testdata/fakeapp").CombinedOutput()
		if err != nil {
			panic(fmt.Sprintf("building fakeapp: %v\n%s", err, out))
		}
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

var nextBase atomic.Int64

func init() { nextBase.Store(int64(21000 + os.Getpid()%2000*10)) }

type harness struct {
	t     *testing.T
	d     *Drover
	app   string // http://host:port of the load balancer
	admin string
	token string
}

func startDrover(t *testing.T, mutate func(*Config)) *harness {
	t.Helper()
	// Each test gets its own copy of the built-in binary and its own port range.
	builtin := filepath.Join(t.TempDir(), "fakeapp")
	raw, _ := os.ReadFile(binV1)
	if err := os.WriteFile(builtin, raw, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{
		Data:  t.TempDir(),
		Admin: AdminConfig{Listen: "127.0.0.1:0"},
		Apps: []*AppConfig{{
			Name: "web", Command: Command{builtin, "--port", "{port}"}, Listen: "127.0.0.1:0",
			Instances: 2, PortBase: int(nextBase.Add(20)),
			Health:       HealthConfig{Path: "/health", Interval: 100 * time.Millisecond},
			StartTimeout: 10 * time.Second, StopTimeout: 3 * time.Second,
		}},
	}
	if mutate != nil {
		mutate(cfg)
	}
	d, err := newDrover(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.stop)
	h := &harness{t: t, d: d, app: "http://" + d.apps[0].addr.String(), admin: "http://" + d.adminAddr.String(), token: cfg.Admin.Token}
	h.waitFor("all instances healthy", func() bool {
		for _, in := range h.status().Instances {
			if in.State != "healthy" {
				return false
			}
		}
		return true
	})
	return h
}

func (h *harness) waitFor(what string, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (h *harness) do(method, path string, body io.Reader) (int, string) {
	h.t.Helper()
	req, _ := http.NewRequest(method, h.admin+path, body)
	if h.token != "" {
		req.Header.Set("Authorization", "Bearer "+h.token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

func (h *harness) status() AppStatus {
	h.t.Helper()
	code, body := h.do(http.MethodGet, "/apps/web", nil)
	if code != 200 {
		h.t.Fatalf("status: %d %s", code, body)
	}
	var st AppStatus
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		h.t.Fatal(err)
	}
	return st
}

// get returns the status code and body of a request through the load balancer.
func (h *harness) get(path string) (int, string) {
	resp, err := http.Get(h.app + path)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// served asks the app n times and returns the set of versions and of processes that answered.
func (h *harness) served(n int) (versions, pids map[string]bool) {
	h.t.Helper()
	versions, pids = map[string]bool{}, map[string]bool{}
	for i := 0; i < n; i++ {
		code, body := h.get("/")
		f := strings.Fields(body)
		if code != 200 || len(f) != 3 {
			h.t.Fatalf("GET /: %d %q", code, body)
		}
		versions[f[0]], pids[f[2]] = true, true
	}
	return
}

// hammer sends requests until stop is closed and returns how many were made and how many failed.
func (h *harness) hammer(stop <-chan struct{}) (total, failed *atomic.Int64, wg *sync.WaitGroup) {
	total, failed, wg = &atomic.Int64{}, &atomic.Int64{}, &sync.WaitGroup{}
	for i := 0; i < 8; i++ {
		path := "/"
		if i%2 == 1 {
			path = "/slow"
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				total.Add(1)
				if code, body := h.get(path); code != 200 {
					failed.Add(1)
					h.t.Logf("request failed: %d %s", code, body)
				}
			}
		}()
	}
	return
}

func TestLoadBalancesAcrossInstances(t *testing.T) {
	h := startDrover(t, nil)
	versions, pids := h.served(20)
	if len(pids) != 2 || !versions["v1"] || len(versions) != 1 {
		t.Fatalf("want 2 processes on v1, got versions %v processes %v", versions, pids)
	}
}

func TestRestartsCrashedInstance(t *testing.T) {
	h := startDrover(t, nil)
	before := h.status().Instances[0]
	if err := syscall.Kill(int(before.PID), syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	h.waitFor("the killed instance to come back", func() bool {
		in := h.status().Instances[0]
		return in.State == "healthy" && in.PID != 0 && in.PID != before.PID
	})
	// While one instance is down the other keeps answering.
	if code, body := h.get("/"); code != 200 {
		t.Fatalf("GET / after crash: %d %s", code, body)
	}
}

func TestUploadRollsOutWithoutFailedRequestsAndRollsBack(t *testing.T) {
	h := startDrover(t, nil)
	stop := make(chan struct{})
	total, failed, wg := h.hammer(stop)

	f, _ := os.Open(binV2)
	defer f.Close()
	code, body := h.do(http.MethodPut, "/apps/web/releases/v2", f)
	if code != 200 {
		t.Fatalf("upload: %d %s", code, body)
	}
	if versions, pids := h.served(20); !versions["v2"] || len(versions) != 1 || len(pids) != 2 {
		t.Fatalf("after upload want 2 processes on v2, got %v %v", versions, pids)
	}
	if st := h.status(); st.Version != "v2" || st.Previous != versionBuiltin {
		t.Fatalf("status after upload: %+v", st)
	}

	if code, body := h.do(http.MethodPost, "/apps/web/rollback", nil); code != 200 {
		t.Fatalf("rollback: %d %s", code, body)
	}
	if versions, _ := h.served(20); !versions["v1"] || len(versions) != 1 {
		t.Fatalf("after rollback want v1, got %v", versions)
	}
	if code, body := h.do(http.MethodPost, "/apps/web/releases/v2/activate", nil); code != 200 {
		t.Fatalf("activate: %d %s", code, body)
	}

	close(stop)
	wg.Wait()
	if failed.Load() != 0 || total.Load() < 20 {
		t.Fatalf("%d of %d requests failed during three rollouts", failed.Load(), total.Load())
	}
	// No instance of an old version is left running on an instance port.
	h.waitFor("old instances to be gone", func() bool {
		st := h.status()
		return st.Version == "v2" && st.Instances[0].Version == "v2" && st.Instances[1].Version == "v2"
	})
}

func TestBadBinaryIsNotActivated(t *testing.T) {
	h := startDrover(t, nil)
	code, body := h.do(http.MethodPut, "/apps/web/releases/broken", strings.NewReader("#!/bin/sh\nexit 1\n"))
	if code != 500 {
		t.Fatalf("want 500 for a binary that exits, got %d %s", code, body)
	}
	if st := h.status(); st.Version != versionBuiltin || st.Previous != "" {
		t.Fatalf("a failed rollout changed the version: %+v", st)
	}
	if versions, pids := h.served(20); !versions["v1"] || len(versions) != 1 || len(pids) != 2 {
		t.Fatalf("after a failed rollout want 2 processes on v1, got %v %v", versions, pids)
	}
	// The release is kept so it can be inspected, and can be deleted.
	if code, _ := h.do(http.MethodDelete, "/apps/web/releases/broken", nil); code != 204 {
		t.Fatalf("delete: %d", code)
	}
	if code, _ := h.do(http.MethodDelete, "/apps/web/releases/"+versionBuiltin, nil); code != 409 {
		t.Fatalf("deleting the current version: want 409, got %d", code)
	}
}

func TestFetchFromURLAndImage(t *testing.T) {
	raw, _ := os.ReadFile(binV2)
	sum := sha256.Sum256(raw)
	var gotImageReq map[string]string
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/bin/app":
			w.Write(raw)
		case "/download": // what an image-file-fetch server exposes
			json.NewDecoder(r.Body).Decode(&gotImageReq)
			w.Write(raw)
		default:
			http.NotFound(w, r)
		}
	}))
	defer src.Close()
	h := startDrover(t, func(c *Config) { c.ImageFetchURL = src.URL })

	post := func(v any) (int, string) {
		b, _ := json.Marshal(v)
		return h.do(http.MethodPost, "/apps/web/releases", bytes.NewReader(b))
	}
	if code, body := post(map[string]any{"url": src.URL + "/bin/app", "version": "bad", "sha256": strings.Repeat("0", 64)}); code != 400 || !strings.Contains(body, "sha256 mismatch") {
		t.Fatalf("wrong checksum: %d %s", code, body)
	}
	if code, body := post(map[string]any{"url": src.URL + "/missing"}); code != 502 {
		t.Fatalf("missing URL: %d %s", code, body)
	}
	if code, body := post(map[string]any{"url": src.URL + "/bin/app", "version": "from-url", "sha256": hex.EncodeToString(sum[:])}); code != 200 {
		t.Fatalf("fetch url: %d %s", code, body)
	}
	if versions, _ := h.served(10); !versions["v2"] || len(versions) != 1 {
		t.Fatalf("after fetch want v2, got %v", versions)
	}
	// An image source is stored without switching to it.
	if code, body := post(map[string]any{"image": "ghcr.io/x/app:2", "path": "/app", "version": "from-image", "activate": false}); code != 201 {
		t.Fatalf("fetch image: %d %s", code, body)
	}
	if gotImageReq["image"] != "ghcr.io/x/app:2" || gotImageReq["path"] != "/app" {
		t.Fatalf("image-file-fetch got %v", gotImageReq)
	}
	if st := h.status(); st.Version != "from-url" {
		t.Fatalf("activate:false switched the version: %+v", st)
	}
	code, body := h.do(http.MethodGet, "/apps/web/releases", nil)
	for _, want := range []string{versionBuiltin, "from-url", "from-image"} {
		if code != 200 || !strings.Contains(body, `"version":"`+want+`"`) {
			t.Fatalf("releases list lacks %s: %d %s", want, code, body)
		}
	}
	if strings.Contains(body, `"version":"bad"`) {
		t.Fatalf("a release with a wrong checksum was stored: %s", body)
	}
}

func TestVersionSurvivesRestartOfDrover(t *testing.T) {
	h := startDrover(t, nil)
	f, _ := os.Open(binV2)
	defer f.Close()
	if code, body := h.do(http.MethodPut, "/apps/web/releases/v2", f); code != 200 {
		t.Fatalf("upload: %d %s", code, body)
	}
	h.d.stop()
	cfg := h.d.cfg
	cfg.Apps[0].PortBase = int(nextBase.Add(20))
	d, err := newDrover(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.start(); err != nil {
		t.Fatal(err)
	}
	defer d.stop()
	h2 := &harness{t: t, d: d, app: "http://" + d.apps[0].addr.String(), admin: "http://" + d.adminAddr.String()}
	h2.waitFor("v2 after restart", func() bool {
		code, body := h2.get("/")
		return code == 200 && strings.HasPrefix(body, "v2 ")
	})
}

func TestAdminToken(t *testing.T) {
	h := startDrover(t, func(c *Config) { c.Admin.Token = "s3cret" })
	h.token = ""
	if code, _ := h.do(http.MethodGet, "/apps", nil); code != 401 {
		t.Fatalf("without token: want 401, got %d", code)
	}
	h.token = "wrong"
	if code, _ := h.do(http.MethodGet, "/apps", nil); code != 401 {
		t.Fatalf("wrong token: want 401, got %d", code)
	}
	h.token = "s3cret"
	if code, _ := h.do(http.MethodGet, "/apps", nil); code != 200 {
		t.Fatalf("right token: want 200, got %d", code)
	}
	// X-Drover-Token works too, also when Authorization carries something else (a proxy's own login).
	for name, header := range map[string]http.Header{
		"X-Drover-Token alone":             {"X-Drover-Token": {"s3cret"}},
		"X-Drover-Token with a basic auth": {"X-Drover-Token": {"s3cret"}, "Authorization": {"Basic dXNlcjpwYXNz"}},
	} {
		req, _ := http.NewRequest(http.MethodGet, h.admin+"/apps", nil)
		req.Header = header
		resp, err := http.DefaultClient.Do(req)
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("%s: %v %v", name, err, resp)
		}
		resp.Body.Close()
	}
	req, _ := http.NewRequest(http.MethodGet, h.admin+"/apps", nil)
	req.Header.Set("X-Drover-Token", "wrong")
	req.Header.Set("Authorization", "Bearer s3cret")
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != 401 {
		t.Fatalf("a wrong X-Drover-Token must not fall back to Authorization: got %d", resp.StatusCode)
	}
}

func TestConfig(t *testing.T) {
	words, err := splitWords(`{bin} --port {port} --name "two words" 'a "b"' c\ d`)
	want := []string{"{bin}", "--port", "{port}", "--name", "two words", `a "b"`, "c d"}
	if err != nil || fmt.Sprint(words) != fmt.Sprint(want) {
		t.Fatalf("splitWords: %q %v", words, err)
	}
	if _, err := splitWords(`a "b`); err == nil {
		t.Fatal("unterminated quote accepted")
	}

	open := &Config{Admin: AdminConfig{Listen: "0.0.0.0:9100"}, Apps: []*AppConfig{{Name: "a", Command: Command{"x"}, Listen: ":1"}}}
	if err := open.normalize(); err == nil || !strings.Contains(err.Error(), "token") {
		t.Fatalf("admin on all interfaces without a token: %v", err)
	}

	c := &Config{Data: t.TempDir(), Apps: []*AppConfig{{Name: "a", Command: Command{"/opt/app", "--port", "{port}"}, Listen: ":1"}}}
	if err := c.normalize(); err != nil {
		t.Fatal(err)
	}
	if a := c.Apps[0]; a.Binary != "/opt/app" || a.Command[0] != "{bin}" {
		t.Fatalf("first word should become the binary: %+v", a)
	}

	overlap := &Config{Apps: []*AppConfig{
		{Name: "a", Command: Command{"x"}, Listen: ":1", PortBase: 9000, Instances: 4},
		{Name: "b", Command: Command{"x"}, Listen: ":2", PortBase: 9004},
	}}
	if err := overlap.normalize(); err == nil || !strings.Contains(err.Error(), "overlapping") {
		t.Fatalf("overlapping ports: %v", err)
	}
}

func TestStartsEmptyAndRunsFirstRelease(t *testing.T) {
	cfg := &Config{Data: t.TempDir(), Admin: AdminConfig{Listen: "127.0.0.1:0"}, Apps: []*AppConfig{{
		Name: "web", Command: Command{"{bin}", "--port", "{port}"}, Listen: "127.0.0.1:0", Instances: 2,
		PortBase: int(nextBase.Add(20)), Health: HealthConfig{Path: "/health", Interval: 100 * time.Millisecond},
		TryDuration: 200 * time.Millisecond,
	}}}
	d, err := newDrover(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.start(); err != nil {
		t.Fatal(err)
	}
	defer d.stop()
	h := &harness{t: t, d: d, app: "http://" + d.apps[0].addr.String(), admin: "http://" + d.adminAddr.String()}
	if code, _ := h.get("/"); code != 503 {
		t.Fatalf("with nothing to run: want 503, got %d", code)
	}
	f, _ := os.Open(binV1)
	defer f.Close()
	if code, body := h.do(http.MethodPut, "/apps/web/releases/first", f); code != 200 {
		t.Fatalf("first release: %d %s", code, body)
	}
	if versions, pids := h.served(20); !versions["v1"] || len(pids) != 2 {
		t.Fatalf("after the first release want 2 processes on v1, got %v %v", versions, pids)
	}
}

// upgrade opens an upgraded (WebSocket-style) connection through the load balancer and returns it with
// the version line the instance sends first.
func (h *harness) upgrade() (net.Conn, *bufio.Reader, string) {
	h.t.Helper()
	conn, err := net.Dial("tcp", strings.TrimPrefix(h.app, "http://"))
	if err != nil {
		h.t.Fatal(err)
	}
	fmt.Fprintf(conn, "GET /ws HTTP/1.1\r\nHost: x\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	r := bufio.NewReader(conn)
	resp, err := http.ReadResponse(r, nil)
	if err != nil || resp.StatusCode != http.StatusSwitchingProtocols {
		h.t.Fatalf("upgrade: %v %+v", err, resp)
	}
	line, err := r.ReadString('\n')
	if err != nil {
		h.t.Fatal(err)
	}
	return conn, r, strings.TrimSpace(line)
}

func TestWebSocketUpgradeAndRollout(t *testing.T) {
	h := startDrover(t, func(c *Config) { c.Apps[0].DrainTimeout = time.Second })
	conn, r, v := h.upgrade()
	defer conn.Close()
	echo := func() error {
		if _, err := fmt.Fprintf(conn, "ping\n"); err != nil {
			return err
		}
		line, err := r.ReadString('\n')
		if err == nil && line != "ping\n" {
			err = fmt.Errorf("echo returned %q", line)
		}
		return err
	}
	if err := echo(); err != nil || v != "v1" {
		t.Fatalf("upgraded connection: version %q, echo %v", v, err)
	}

	// A rollout waits drain_timeout for the open connection, then stops the old instance: the connection
	// ends, and a client that reconnects reaches the new version.
	f, _ := os.Open(binV2)
	defer f.Close()
	if code, body := h.do(http.MethodPut, "/apps/web/releases/v2", f); code != 200 {
		t.Fatalf("upload: %d %s", code, body)
	}
	if err := echo(); err == nil {
		t.Fatal("the connection to the old instance is still open after the rollout")
	}
	conn2, _, v2 := h.upgrade()
	defer conn2.Close()
	if v2 != "v2" {
		t.Fatalf("after the rollout a new connection reached %q", v2)
	}
}

func TestLogs(t *testing.T) {
	h := startDrover(t, nil)
	code, body := h.do(http.MethodGet, "/apps/web/logs", nil)
	if code != 200 || !strings.Contains(body, "[web#0] fakeapp v1 listening on") || !strings.Contains(body, "[web#1] fakeapp v1") {
		t.Fatalf("logs: %d %q", code, body)
	}
	code, body = h.do(http.MethodGet, "/apps/web/logs?instance=1", nil)
	if code != 200 || strings.Contains(body, "[web#0]") || !strings.Contains(body, "[web#1]") {
		t.Fatalf("logs of instance 1: %d %q", code, body)
	}
	if code, body = h.do(http.MethodGet, "/apps/web/logs?tail=1", nil); code != 200 || strings.Count(body, "\n") != 1 {
		t.Fatalf("tail=1: %d %q", code, body)
	}
	if code, _ = h.do(http.MethodGet, "/apps/web/logs?instance=7", nil); code != 400 {
		t.Fatalf("instance out of range: want 400, got %d", code)
	}

	// A follower gets lines printed after it connected: here, the instances a restart brings up.
	resp, err := http.Get(h.admin + "/apps/web/logs?follow=true&tail=0")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	lines := make(chan string, 100)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	if code, body := h.do(http.MethodPost, "/apps/web/restart", nil); code != 200 {
		t.Fatalf("restart: %d %s", code, body)
	}
	sawStart, sawEvent := false, false
	timeout := time.After(10 * time.Second)
	for !sawStart || !sawEvent {
		select {
		case l := <-lines:
			sawStart = sawStart || strings.Contains(l, "fakeapp v1 listening on")
			sawEvent = sawEvent || strings.Contains(l, "[web] rollout builtin -> builtin")
		case <-timeout:
			t.Fatalf("follower did not get the new lines (instance line %v, rollout event %v)", sawStart, sawEvent)
		}
	}
	// Stopping drover ends the follower instead of waiting for it.
	began := time.Now()
	h.d.stop()
	if time.Since(began) > 8*time.Second {
		t.Fatalf("stop waited %s for a log follower", time.Since(began))
	}
}

func TestFailedRolloutSaysWhy(t *testing.T) {
	h := startDrover(t, nil)
	script := "#!/bin/sh\necho 'cannot open config: boom'\nexit 1\n"
	code, body := h.do(http.MethodPut, "/apps/web/releases/broken", strings.NewReader(script))
	if code != 500 || !strings.Contains(body, "cannot open config: boom") {
		t.Fatalf("want 500 with the instance's output, got %d %s", code, body)
	}
}

func TestMetrics(t *testing.T) {
	h := startDrover(t, func(c *Config) { c.Metrics.Listen = "127.0.0.1:0"; c.Admin.Token = "s3cret" })
	h.served(5)
	f, _ := os.Open(binV2)
	defer f.Close()
	if code, body := h.do(http.MethodPut, "/apps/web/releases/v2", f); code != 200 {
		t.Fatalf("upload: %d %s", code, body)
	}
	h.do(http.MethodPut, "/apps/web/releases/broken", strings.NewReader("#!/bin/sh\nexit 1\n"))

	// The metrics listener needs no token; the admin API's /metrics does.
	resp, err := http.Get("http://" + h.d.metricsAddr.String() + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	body := string(raw)
	for _, want := range []string{
		`drover_requests_total{app="web",code="200"} 5`,
		`drover_request_duration_seconds_count{app="web"} 5`,
		`drover_request_duration_seconds_bucket{app="web",le="+Inf"} 5`,
		`drover_rollouts_total{app="web",result="success"} 1`,
		`drover_rollouts_total{app="web",result="failed"} 1`,
		`drover_instances{app="web"} 2`,
		`drover_healthy_instances{app="web"} 2`,
		`drover_version_info{app="web",version="v2"} 1`,
		`drover_inflight_requests{app="web",instance="0"} 0`,
		`drover_instance_exits_total{app="web"} 2`,
		`# TYPE drover_request_duration_seconds histogram`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics lack %q", want)
		}
	}
	if t.Failed() {
		t.Log(body)
	}
	if resp, _ := http.Get("http://" + h.d.metricsAddr.String() + "/apps"); resp.StatusCode != 404 {
		t.Fatalf("the metrics listener serves more than /metrics: %d", resp.StatusCode)
	}
	if resp, _ := http.Get(h.admin + "/metrics"); resp.StatusCode != 401 {
		t.Fatalf("admin /metrics without the token: want 401, got %d", resp.StatusCode)
	}
	if code, body := h.do(http.MethodGet, "/metrics", nil); code != 200 || !strings.Contains(body, "drover_build_info") {
		t.Fatalf("admin /metrics with the token: %d", code)
	}
}
