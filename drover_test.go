package main

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
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

// Instance ports for the tests stay below 32768: from there up Linux hands ports to outgoing connections,
// and an instance cannot bind a port that a client connection happens to hold.
func init() { nextBase.Store(int64(10000 + os.Getpid()%1000*20)) }

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
	// Only the version is checked while the load runs: with least-connections balancing, a burst of quick
	// requests can all go to the instance that has fewer slow ones open.
	if versions, _ := h.served(20); !versions["v2"] || len(versions) != 1 {
		t.Fatalf("after upload want v2, got %v", versions)
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
	if versions, pids := h.served(20); !versions["v2"] || len(versions) != 1 || len(pids) != 2 {
		t.Fatalf("with no other load want 2 processes on v2, got %v %v", versions, pids)
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
		`drover_env_pending{app="web"} 0`,
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

// envOf returns the value of a variable as an instance sees it, asked once per instance.
func (h *harness) envOf(name string) map[string]bool {
	h.t.Helper()
	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		code, body := h.get("/env?name=" + name)
		if code != 200 {
			h.t.Fatalf("GET /env: %d %s", code, body)
		}
		seen[strings.TrimSpace(body)] = true
	}
	return seen
}

func TestEnvFileAndOverrides(t *testing.T) {
	envFile := filepath.Join(t.TempDir(), "app.env")
	os.WriteFile(envFile, []byte("# settings\nGREETING=from-file\nexport QUOTED=\"two words\"\nPORT=1\n"), 0o600)
	h := startDrover(t, func(c *Config) {
		c.Apps[0].EnvFile = envFile
		c.Apps[0].Env = map[string]string{"GREETING": "from-config", "ONLY_CONFIG": "yes"}
	})
	one := func(name, want string) {
		t.Helper()
		if got := h.envOf(name); len(got) != 1 || !got[want] {
			t.Fatalf("%s: want %q on every instance, got %v", name, want, got)
		}
	}
	// The file wins over the config; PORT in the file is ignored, or the instances would not be up at all.
	one("GREETING", "from-file")
	one("QUOTED", "two words")
	one("ONLY_CONFIG", "yes")

	// Editing the file by hand changes nothing until a restart.
	if h.status().EnvPending {
		t.Fatal("env_pending right after start")
	}
	os.WriteFile(envFile, []byte("GREETING=edited\n"), 0o600)
	one("GREETING", "from-file")
	if !h.status().EnvPending {
		t.Fatal("a hand-edited env file should show as env_pending")
	}
	if code, body := h.do(http.MethodPost, "/apps/web/restart", nil); code != 200 {
		t.Fatalf("restart: %d %s", code, body)
	}
	one("GREETING", "edited")
	if h.status().EnvPending {
		t.Fatal("env_pending after the restart that applied the file")
	}

	patch := func(path, body string) (int, string) {
		return h.do(http.MethodPatch, path, strings.NewReader(body))
	}
	// An override wins over the file, is in memory only, and shows up by name, never by value.
	code, body := patch("/apps/web/env", `{"set":{"GREETING":"override","SECRET":"hunter2"}}`)
	if code != 200 || strings.Contains(body, "hunter2") || !strings.Contains(body, `"overrides":["GREETING","SECRET"]`) {
		t.Fatalf("set override: %d %s", code, body)
	}
	one("GREETING", "override")
	one("SECRET", "hunter2")
	if st := h.status(); fmt.Sprint(st.EnvOverrides) != "[GREETING SECRET]" {
		t.Fatalf("status lacks the override names: %+v", st.EnvOverrides)
	}
	if raw, _ := os.ReadFile(envFile); strings.Contains(string(raw), "hunter2") {
		t.Fatal("an override was written to the env file")
	}
	if _, err := os.Stat(filepath.Join(h.d.cfg.Data, "web", "env")); err == nil {
		t.Fatal("overrides were stored in the data directory")
	}
	if code, body := patch("/apps/web/env", `{"unset":["GREETING"]}`); code != 200 {
		t.Fatalf("unset override: %d %s", code, body)
	}
	one("GREETING", "edited")
	one("SECRET", "hunter2")

	// Editing the file through the API: comments and other lines stay.
	os.WriteFile(envFile, []byte("# keep me\nGREETING=edited\nOLD=1\n"), 0o600)
	if code, body := patch("/apps/web/env/file", `{"set":{"GREETING":"via api","NEW":"x"},"unset":["OLD"]}`); code != 200 {
		t.Fatalf("patch file: %d %s", code, body)
	}
	if raw, _ := os.ReadFile(envFile); string(raw) != "# keep me\nGREETING=\"via api\"\nNEW=x\n" {
		t.Fatalf("env file after patch: %q", raw)
	}
	one("GREETING", "via api")
	one("NEW", "x")

	// Replacing the whole file, without a restart: nothing changes until one.
	if code, body := h.do(http.MethodPut, "/apps/web/env/file?restart=false", strings.NewReader("GREETING=replaced\n")); code != 200 {
		t.Fatalf("put file: %d %s", code, body)
	}
	one("GREETING", "via api")
	if _, body := h.do(http.MethodGet, "/apps/web/env", nil); !h.status().EnvPending || !strings.Contains(body, `"pending":true`) {
		t.Fatalf("a change stored with restart=false should show as pending: %s", body)
	}
	h.do(http.MethodPost, "/apps/web/restart", nil)
	if _, body := h.do(http.MethodGet, "/apps/web/env", nil); h.status().EnvPending || !strings.Contains(body, `"pending":false`) {
		t.Fatalf("pending after the restart: %s", body)
	}
	one("GREETING", "replaced")
	one("NEW", "")

	// Clearing the overrides.
	if code, body := h.do(http.MethodDelete, "/apps/web/env", nil); code != 200 || !strings.Contains(body, `"overrides":[]`) {
		t.Fatalf("clear overrides: %d %s", code, body)
	}
	one("SECRET", "")

	// Refused: reserved names, bad names, a file that does not parse.
	for _, bad := range []string{`{"set":{"PORT":"1"}}`, `{"set":{"A B":"1"}}`, `{}`} {
		if code, _ := patch("/apps/web/env", bad); code != 400 {
			t.Fatalf("override %s: want 400, got %d", bad, code)
		}
	}
	if code, _ := h.do(http.MethodPut, "/apps/web/env/file", strings.NewReader("not a variable\n")); code != 400 {
		t.Fatalf("unparsable file: want 400, got %d", code)
	}
	if code, _ := h.do(http.MethodPut, "/apps/web/env/file", strings.NewReader("DROVER_APP=x\n")); code != 400 {
		t.Fatalf("reserved name in the file: want 400, got %d", code)
	}
	one("GREETING", "replaced")
}

func TestEnvChangeIsUndoneWhenInstancesDoNotStart(t *testing.T) {
	envFile := filepath.Join(t.TempDir(), "app.env")
	os.WriteFile(envFile, []byte("GREETING=good\n"), 0o600)
	h := startDrover(t, func(c *Config) { c.Apps[0].EnvFile = envFile })

	// FAKEAPP_FAIL makes the test app exit at start.
	code, body := h.do(http.MethodPatch, "/apps/web/env", strings.NewReader(`{"set":{"FAKEAPP_FAIL":"1"}}`))
	if code != 500 || !strings.Contains(body, "change was undone") {
		t.Fatalf("bad override: want 500, got %d %s", code, body)
	}
	if st := h.status(); len(st.EnvOverrides) != 0 {
		t.Fatalf("the failed override is still set: %v", st.EnvOverrides)
	}
	code, body = h.do(http.MethodPatch, "/apps/web/env/file", strings.NewReader(`{"set":{"FAKEAPP_FAIL":"1"}}`))
	if code != 500 {
		t.Fatalf("bad file change: want 500, got %d %s", code, body)
	}
	if raw, _ := os.ReadFile(envFile); string(raw) != "GREETING=good\n" {
		t.Fatalf("the env file was not put back: %q", raw)
	}
	if got := h.envOf("GREETING"); len(got) != 1 || !got["good"] {
		t.Fatalf("after two failed changes want the old instances serving, got %v", got)
	}

	// A bad value stored without a restart is not tried, so not undone. The next rollout, here a new
	// version, fails on it, and the error points at the environment.
	if code, body := h.do(http.MethodPatch, "/apps/web/env?restart=false", strings.NewReader(`{"set":{"FAKEAPP_FAIL":"1"}}`)); code != 200 {
		t.Fatalf("store without restart: %d %s", code, body)
	}
	if got := h.envOf("GREETING"); !h.status().EnvPending || !got["good"] {
		t.Fatalf("stored change: want pending and the old instances serving, got %v", got)
	}
	f, _ := os.Open(binV2)
	defer f.Close()
	code, body = h.do(http.MethodPut, "/apps/web/releases/v2", f)
	if code != 500 || !strings.Contains(body, "may be a variable and not the binary") {
		t.Fatalf("rollout on a bad pending environment: %d %s", code, body)
	}
	if st := h.status(); st.Version != versionBuiltin || !st.EnvPending || fmt.Sprint(st.EnvOverrides) != "[FAKEAPP_FAIL]" {
		t.Fatalf("after that failed rollout: %+v", st)
	}
	// Fixing the environment lets the same version roll out.
	if code, body := h.do(http.MethodDelete, "/apps/web/env?restart=false", nil); code != 200 {
		t.Fatalf("clear: %d %s", code, body)
	}
	if code, body := h.do(http.MethodPost, "/apps/web/releases/v2/activate", nil); code != 200 || strings.Contains(body, "env_pending") {
		t.Fatalf("activate after the fix: %d %s", code, body)
	}

	// An app without env_file: overrides work, file calls are refused.
	h2 := startDrover(t, nil)
	if code, _ := h2.do(http.MethodPatch, "/apps/web/env/file", strings.NewReader(`{"set":{"A":"1"}}`)); code != 409 {
		t.Fatalf("file change without env_file: want 409, got %d", code)
	}
	if code, _ := h2.do(http.MethodPatch, "/apps/web/env", strings.NewReader(`{"set":{"A":"1"}}`)); code != 200 {
		t.Fatalf("override without env_file: want 200, got %d", code)
	}
}

func TestEnvFileText(t *testing.T) {
	vars, names, err := parseEnv([]byte("# c\n\nA=1\nexport B = two words \nC='single $x'\nD=\"q\\\"uo\\nte\"\nE=\n"))
	want := []string{"A=1", "B=two words", "C=single $x", "D=q\"uo\nte", "E="}
	if err != nil || fmt.Sprint(vars) != fmt.Sprint(want) || fmt.Sprint(names) != "[A B C D E]" {
		t.Fatalf("parseEnv: %q %v %v", vars, names, err)
	}
	if _, _, err := parseEnv([]byte("A=1\nnope\n")); err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("a bad line should be reported with its number: %v", err)
	}
	// Whatever is written reads back as the same value.
	for _, v := range []string{"plain", "", " padded ", "two words", `quo"te`, "new\nline", `back\slash`, "has#hash", "it's"} {
		_, got, ok, err := parseEnvLine(formatEnvLine("K", v))
		if err != nil || !ok || got != v {
			t.Fatalf("round trip of %q gave %q (%v)", v, got, err)
		}
	}
}

// --- folder apps

type entry struct {
	name, body string
	mode       int64
	link       string // a symlink to this target, when set
}

func tarOf(t *testing.T, gz bool, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	var w io.Writer = &buf
	var zw *gzip.Writer
	if gz {
		zw = gzip.NewWriter(&buf)
		w = zw
	}
	tw := tar.NewWriter(w)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Mode: e.mode, Size: int64(len(e.body)), Typeflag: tar.TypeReg}
		if hdr.Mode == 0 {
			hdr.Mode = 0o644
		}
		switch {
		case e.link != "":
			hdr.Typeflag, hdr.Linkname, hdr.Size = tar.TypeSymlink, e.link, 0
		case strings.HasSuffix(e.name, "/"):
			hdr.Typeflag, hdr.Mode, hdr.Size = tar.TypeDir, 0o755, 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(e.body))
	}
	tw.Close()
	if zw != nil {
		zw.Close()
	}
	return buf.Bytes()
}

func zipOf(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		hdr := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		mode := fs.FileMode(0o644)
		if e.mode != 0 {
			mode = fs.FileMode(e.mode)
		}
		hdr.SetMode(mode)
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(e.body))
	}
	zw.Close()
	return buf.Bytes()
}

// launcher is a small script that becomes the test app: the "server" of a folder release, without putting
// a whole binary into every test archive.
func launcher(bin string) string { return "#!/bin/sh\nexec " + bin + " \"$@\"\n" }

// startFolderApp runs the test app from a folder: {dir}/server, with a version.txt beside it.
func startFolderApp(t *testing.T, mutate func(*Config)) (*harness, string) {
	t.Helper()
	builtin := filepath.Join(t.TempDir(), "site")
	os.MkdirAll(builtin, 0o755)
	os.WriteFile(filepath.Join(builtin, "server"), []byte(launcher(binV1)), 0o755)
	os.WriteFile(filepath.Join(builtin, "version.txt"), []byte("one"), 0o644)
	h := startDrover(t, func(c *Config) {
		a := c.Apps[0]
		a.Command, a.Folder = Command{"{dir}/server", "--port", "{port}"}, builtin
		a.Env = map[string]string{"DEPS": "{dir}/deps"}
		if mutate != nil {
			mutate(c)
		}
	})
	return h, builtin
}

func (h *harness) body(path string) string {
	h.t.Helper()
	code, body := h.get(path)
	if code != 200 {
		h.t.Fatalf("GET %s: %d %s", path, code, body)
	}
	return strings.TrimSpace(body)
}

func TestFolderReleases(t *testing.T) {
	h, builtin := startFolderApp(t, nil)
	v2 := launcher(binV2)
	if got := h.body("/file?name=version.txt"); got != "one" || h.body("/cwd") != builtin {
		t.Fatalf("built-in folder: version.txt %q, cwd %q (want %q)", got, h.body("/cwd"), builtin)
	}
	if got := h.body("/env?name=DEPS"); got != builtin+"/deps" {
		t.Fatalf("{dir} in an env value: %q", got)
	}
	if got := h.body("/env?name=DROVER_DIR"); got != builtin {
		t.Fatalf("DROVER_DIR: %q", got)
	}

	// A tar.gz upload becomes the folder of the new version.
	archive := tarOf(t, true,
		entry{name: "./"}, entry{name: "./server", body: v2, mode: 0o755},
		entry{name: "./version.txt", body: "two"}, entry{name: "./static/"},
		entry{name: "./static/app.js", body: "js"}, entry{name: "./current.txt", link: "version.txt"})
	code, body := h.do(http.MethodPut, "/apps/web/releases/v2", bytes.NewReader(archive))
	if code != 200 || !strings.Contains(body, `"folder":true`) {
		t.Fatalf("upload tar.gz: %d %s", code, body)
	}
	dir := filepath.Join(h.d.cfg.Data, "web", "releases", "v2", "tree")
	if h.body("/file?name=version.txt") != "two" || h.body("/file?name=static/app.js") != "js" ||
		h.body("/file?name=current.txt") != "two" || h.body("/cwd") != dir {
		t.Fatalf("v2 folder is not what was uploaded (cwd %s)", h.body("/cwd"))
	}
	if versions, pids := h.served(20); !versions["v2"] || len(versions) != 1 || len(pids) != 2 {
		t.Fatalf("after upload want 2 processes on v2, got %v %v", versions, pids)
	}
	if _, body := h.do(http.MethodGet, "/apps/web/releases", nil); !strings.Contains(body, `"version":"v2","size":`) || !strings.Contains(body, `"files":3`) {
		t.Fatalf("release list: %s", body)
	}

	// Back to the folder from the image, then a zip with the build in a subfolder.
	if code, body := h.do(http.MethodPost, "/apps/web/rollback", nil); code != 200 {
		t.Fatalf("rollback: %d %s", code, body)
	}
	if h.body("/file?name=version.txt") != "one" {
		t.Fatal("rollback did not go back to the built-in folder")
	}
	z := zipOf(t, entry{name: "repo-main/README.md", body: "not part of the build"},
		entry{name: "repo-main/dist/server", body: v2, mode: 0o755},
		entry{name: "repo-main/dist/version.txt", body: "three"})
	if code, body := h.do(http.MethodPut, "/apps/web/releases/v3?subdir=repo-main/dist", bytes.NewReader(z)); code != 200 {
		t.Fatalf("upload zip with subdir: %d %s", code, body)
	}
	if h.body("/file?name=version.txt") != "three" {
		t.Fatal("v3 is not the subfolder of the zip")
	}
	if _, err := os.Stat(filepath.Join(h.d.cfg.Data, "web", "releases", "v3", "tree", "README.md")); err == nil {
		t.Fatal("a file outside subdir was kept")
	}

	// A folder that cannot run is not activated, like a binary that cannot.
	bad := tarOf(t, false, entry{name: "server", body: "#!/bin/sh\necho broken build\nexit 1\n", mode: 0o755})
	if code, body := h.do(http.MethodPut, "/apps/web/releases/bad", bytes.NewReader(bad)); code != 500 || !strings.Contains(body, "broken build") {
		t.Fatalf("a folder that does not start: %d %s", code, body)
	}
	if h.body("/file?name=version.txt") != "three" {
		t.Fatal("a failed folder rollout changed what is served")
	}
	if code, _ := h.do(http.MethodDelete, "/apps/web/releases/v2", nil); code != 204 {
		t.Fatalf("delete a folder version: %d", code)
	}
}

func TestFolderArchivesThatAreRefused(t *testing.T) {
	h, _ := startFolderApp(t, func(c *Config) { c.MaxBinaryBytes = 1 << 20 })
	outside := filepath.Join(h.d.cfg.Data, "escaped")
	put := func(name string, body []byte) (int, string) {
		return h.do(http.MethodPut, "/apps/web/releases/"+name, bytes.NewReader(body))
	}
	for name, tc := range map[string]struct {
		archive []byte
		want    string
	}{
		"abs-link":       {tarOf(t, false, entry{name: "server", body: "x"}, entry{name: "etc", link: "/etc"}), "points outside the release"},
		"up-link":        {tarOf(t, false, entry{name: "server", body: "x"}, entry{name: "a/up", link: "../../x"}), "points outside the release"},
		"empty":          {tarOf(t, false, entry{name: "only-a-dir/"}), "no files"},
		"too-big":        {tarOf(t, true, entry{name: "big", body: strings.Repeat("0", 2<<20)}), "max_binary_bytes"},
		"not-an-archive": {[]byte("just some text, not an archive"), "not a readable"},
		"xz":             {[]byte{0xfd, '7', 'z', 'X', 'Z', 0, 0, 0}, "not supported"},
		// A file "inside" a link: writing it must not follow the link.
		"through-link": {tarOf(t, false, entry{name: "server", body: "x"}, entry{name: "link", link: "target"}, entry{name: "link/through", body: "z"}), "both a link and a folder"},
	} {
		if code, body := put(name, tc.archive); code != 400 || !strings.Contains(body, tc.want) {
			t.Errorf("%s: want 400 with %q, got %d %s", name, tc.want, code, body)
		}
	}
	// Names that try to leave the folder are kept inside it; nothing is written outside.
	sneaky := tarOf(t, false, entry{name: "../../escaped", body: "x"}, entry{name: "/abs", body: "y"})
	code, body := h.do(http.MethodPut, "/apps/web/releases/sneaky?activate=false", bytes.NewReader(sneaky))
	tree := filepath.Join(h.d.cfg.Data, "web", "releases", "sneaky", "tree")
	if _, err := os.Stat(filepath.Join(tree, "escaped")); code != 201 || err != nil {
		t.Fatalf("sneaky names: %d %s (%v)", code, body, err)
	}
	if _, err := os.Stat(outside); err == nil {
		t.Fatal("an entry was written outside the release")
	}
	entries, _ := os.ReadDir(filepath.Join(h.d.cfg.Data, "web", "releases"))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".incoming") {
			t.Fatalf("a refused release left %s behind", e.Name())
		}
	}
	if code, body := h.do(http.MethodPut, "/apps/web/releases/x?subdir=nope", bytes.NewReader(tarOf(t, false, entry{name: "server", body: "x"}))); code != 400 || !strings.Contains(body, "no files") {
		t.Fatalf("a subdir that is not in the archive: %d %s", code, body)
	}

	// A file app does not take folders, and the other way round.
	h2 := startDrover(t, nil)
	if code, body := h2.do(http.MethodPut, "/apps/web/releases/x?subdir=dist", strings.NewReader("x")); code != 400 || !strings.Contains(body, "folder apps") {
		t.Fatalf("subdir on a file app: %d %s", code, body)
	}
	if code, body := h2.do(http.MethodPost, "/apps/web/releases", strings.NewReader(`{"git":"https://example.com/x.git"}`)); code != 400 || !strings.Contains(body, "single binary") {
		t.Fatalf("git on a file app: %d %s", code, body)
	}
	bad := &Config{Apps: []*AppConfig{{Name: "a", Command: Command{"{dir}/x"}, Binary: "/x", Listen: ":1"}}}
	if err := bad.normalize(); err == nil || !strings.Contains(err.Error(), "folder app") {
		t.Fatalf("binary: with {dir}: %v", err)
	}
}

func TestFolderFromGitImageAndURL(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	gitSchemes["file"] = true
	defer delete(gitSchemes, "file")
	v2 := launcher(binV2)

	// A repository with the build in build/out, a tag on the first commit and a second commit on top.
	repo := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(version string) {
		os.MkdirAll(filepath.Join(repo, "build", "out"), 0o755)
		os.WriteFile(filepath.Join(repo, "README.md"), []byte("source"), 0o644)
		os.WriteFile(filepath.Join(repo, "build", "out", "server"), []byte(v2), 0o755)
		os.WriteFile(filepath.Join(repo, "build", "out", "version.txt"), []byte(version), 0o644)
		run("add", "-A")
		run("commit", "-q", "-m", version)
	}
	run("init", "-q", "-b", "main")
	write("git-first")
	run("tag", "v1.0")
	first := run("rev-parse", "HEAD")
	write("git-second")

	h, _ := startFolderApp(t, nil)
	post := func(v any) (int, string) {
		b, _ := json.Marshal(v)
		return h.do(http.MethodPost, "/apps/web/releases", bytes.NewReader(b))
	}
	url := "file://" + repo
	if code, body := post(map[string]any{"git": url, "subdir": "build/out", "version": "head"}); code != 200 {
		t.Fatalf("git HEAD: %d %s", code, body)
	}
	if h.body("/file?name=version.txt") != "git-second" {
		t.Fatal("git HEAD did not give the latest commit")
	}
	tree := filepath.Join(h.d.cfg.Data, "web", "releases", "head", "tree")
	for _, gone := range []string{".git", "README.md", "build"} {
		if _, err := os.Stat(filepath.Join(tree, gone)); err == nil {
			t.Fatalf("%s is in the release, outside the subdir", gone)
		}
	}
	if code, body := post(map[string]any{"git": url, "ref": "v1.0", "subdir": "build/out", "version": "tag"}); code != 200 {
		t.Fatalf("git tag: %d %s", code, body)
	}
	if h.body("/file?name=version.txt") != "git-first" {
		t.Fatal("git ref v1.0 did not give the tagged commit")
	}
	if _, body := h.do(http.MethodGet, "/apps/web/releases", nil); !strings.Contains(body, "@"+first+":build/out") {
		t.Fatalf("the release does not record the commit: %s", body)
	}
	for name, tc := range map[string]struct {
		req  map[string]any
		code int
	}{
		"missing subdir": {map[string]any{"git": url, "subdir": "nope"}, 400},
		"missing ref":    {map[string]any{"git": url, "ref": "no-such-branch"}, 502},
		"option as ref":  {map[string]any{"git": url, "ref": "--upload-pack=x"}, 400},
		"ssh url":        {map[string]any{"git": "ssh://git@example.com/x.git"}, 400},
		"creds in url":   {map[string]any{"git": "https://u:p@example.com/x.git"}, 400},
	} {
		if code, body := post(tc.req); code != tc.code {
			t.Errorf("%s: want %d, got %d %s", name, tc.code, code, body)
		}
	}

	// A folder inside a container image, through image-file-fetch, and an archive by URL.
	var imageReq map[string]string
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/download":
			json.NewDecoder(r.Body).Decode(&imageReq)
			w.Header().Set("Content-Type", "application/x-tar")
			w.Write(tarOf(t, false, entry{name: "server", body: v2, mode: 0o755}, entry{name: "version.txt", body: "from-image"}))
		case "/site.tar.gz":
			w.Write(tarOf(t, true, entry{name: "site/server", body: v2, mode: 0o755}, entry{name: "site/version.txt", body: "from-url"}))
		}
	}))
	defer src.Close()
	h.d.cfg.ImageFetchURL = src.URL
	if code, body := post(map[string]any{"image": "ghcr.io/me/site:2", "path": "/var/www", "version": "img"}); code != 200 {
		t.Fatalf("image folder: %d %s", code, body)
	}
	if imageReq["archive"] != "tar" || imageReq["path"] != "/var/www" || h.body("/file?name=version.txt") != "from-image" {
		t.Fatalf("image folder: asked %v, serving %q", imageReq, h.body("/file?name=version.txt"))
	}
	if code, body := post(map[string]any{"url": src.URL + "/site.tar.gz", "subdir": "site", "version": "url"}); code != 200 {
		t.Fatalf("archive URL: %d %s", code, body)
	}
	if h.body("/file?name=version.txt") != "from-url" {
		t.Fatal("archive URL with subdir did not become the release")
	}
}

func TestTarGzDirRoundTrip(t *testing.T) {
	src := t.TempDir()
	os.MkdirAll(filepath.Join(src, "a", "b"), 0o755)
	os.WriteFile(filepath.Join(src, "a", "b", "f.txt"), []byte("hello"), 0o640)
	os.WriteFile(filepath.Join(src, "run.sh"), []byte("#!/bin/sh\n"), 0o755)
	os.Symlink("a/b/f.txt", filepath.Join(src, "link"))
	var buf bytes.Buffer
	if err := tarGzDir(src, &buf); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "a.tgz")
	os.WriteFile(archive, buf.Bytes(), 0o644)
	dest := t.TempDir()
	if err := extractArchive(archive, dest, "", 1<<20); err != nil {
		t.Fatal(err)
	}
	if files, size, err := checkTree(dest, 1<<20); err != nil || files != 2 || size != 15 {
		t.Fatalf("checkTree: %d files, %d bytes, %v", files, size, err)
	}
	if raw, _ := os.ReadFile(filepath.Join(dest, "link")); string(raw) != "hello" {
		t.Fatalf("the link does not lead to the file: %q", raw)
	}
	if st, _ := os.Stat(filepath.Join(dest, "run.sh")); st == nil || st.Mode().Perm()&0o100 == 0 {
		t.Fatal("the executable bit was lost")
	}
}

func TestURLSourceThatNeedsAuthorization(t *testing.T) {
	payload, _ := os.ReadFile(binV2)
	// The storage host a download is redirected to. It must never see the caller's credentials.
	var storageSaw http.Header
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		storageSaw = r.Header.Clone()
		w.Write(payload)
	}))
	defer storage.Close()
	// The site that needs a token or a login.
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, basic := r.BasicAuth()
		token := r.Header.Get("Private-Token") == "t0ken" || r.Header.Get("Authorization") == "Bearer t0ken"
		if !token && !(basic && user == "ci" && pass == "s3cret") {
			http.Error(w, "login needed", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/file":
			w.Write(payload)
		case "/same-host":
			http.Redirect(w, r, "/file", http.StatusFound)
		case "/other-host":
			http.Redirect(w, r, storage.URL+"/blob", http.StatusFound)
		}
	}))
	defer site.Close()

	h := startDrover(t, nil)
	n := 0
	post := func(fields map[string]any) (int, string) {
		n++
		fields["version"], fields["activate"] = fmt.Sprintf("r%d", n), false
		b, _ := json.Marshal(fields)
		return h.do(http.MethodPost, "/apps/web/releases", bytes.NewReader(b))
	}
	if code, body := post(map[string]any{"url": site.URL + "/file"}); code != 502 || !strings.Contains(body, "401") {
		t.Fatalf("without credentials: want 502 naming the 401, got %d %s", code, body)
	}
	for name, fields := range map[string]map[string]any{
		"custom header":        {"url": site.URL + "/file", "headers": map[string]string{"PRIVATE-TOKEN": "t0ken"}},
		"bearer token":         {"url": site.URL + "/file", "headers": map[string]string{"Authorization": "Bearer t0ken"}},
		"username, password":   {"url": site.URL + "/file", "username": "ci", "password": "s3cret"},
		"login in the URL":     {"url": strings.Replace(site.URL, "://", "://ci:s3cret@", 1) + "/file"},
		"redirect, same host":  {"url": site.URL + "/same-host", "headers": map[string]string{"PRIVATE-TOKEN": "t0ken"}},
		"redirect, other host": {"url": site.URL + "/other-host", "headers": map[string]string{"PRIVATE-TOKEN": "t0ken"}},
		"redirect with login":  {"url": site.URL + "/other-host", "username": "ci", "password": "s3cret"},
	} {
		storageSaw = nil
		if code, body := post(fields); code != 201 {
			t.Errorf("%s: want 201, got %d %s", name, code, body)
		}
		if storageSaw != nil && (storageSaw.Get("Private-Token") != "" || storageSaw.Get("Authorization") != "") {
			t.Errorf("%s: the host the download was redirected to received the credentials: %v", name, storageSaw)
		}
	}
	// Nothing that was sent as a credential is kept.
	filepath.WalkDir(h.d.cfg.Data, func(p string, d fs.DirEntry, _ error) error {
		if d != nil && d.Name() == "meta.json" {
			if raw, _ := os.ReadFile(p); bytes.Contains(raw, []byte("t0ken")) || bytes.Contains(raw, []byte("s3cret")) {
				t.Errorf("%s holds a credential: %s", p, raw)
			}
		}
		return nil
	})
	if _, body := h.do(http.MethodGet, "/apps/web/logs?tail=1000", nil); strings.Contains(body, "t0ken") || strings.Contains(body, "s3cret") {
		t.Errorf("the logs hold a credential: %s", body)
	}
	for name, fields := range map[string]map[string]any{
		"header on an image source": {"image": "x/y:1", "path": "/a", "headers": map[string]string{"A": "b"}},
		"Host header":               {"url": site.URL + "/file", "headers": map[string]string{"Host": "evil"}},
		"line break in a value":     {"url": site.URL + "/file", "headers": map[string]string{"A": "b\r\nC: d"}},
		"space in a name":           {"url": site.URL + "/file", "headers": map[string]string{"A B": "c"}},
	} {
		if code, body := post(fields); code != 400 {
			t.Errorf("%s: want 400, got %d %s", name, code, body)
		}
	}
}
