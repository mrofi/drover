package main

import (
	"fmt"
	"io"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Request duration buckets, in seconds.
var durationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

// appMetrics holds the counters of one app. The gauges (healthy instances, open requests, version) are
// read from the app itself when the metrics are written.
type appMetrics struct {
	mu      sync.Mutex
	codes   map[int]uint64
	buckets []uint64 // requests at or under each durationBuckets entry (not cumulative)
	sum     float64
	count   uint64

	retries      atomic.Uint64
	exits        atomic.Uint64
	rolloutsOK   atomic.Uint64
	rolloutsFail atomic.Uint64
}

func newAppMetrics() *appMetrics {
	return &appMetrics{codes: map[int]uint64{}, buckets: make([]uint64, len(durationBuckets))}
}

// observe records one finished request. Upgraded connections (101) are counted but kept out of the
// duration histogram: they stay open for as long as the client likes.
func (m *appMetrics) observe(code int, took time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.codes[code]++
	if code == 101 {
		return
	}
	s := took.Seconds()
	m.sum += s
	m.count++
	for i, le := range durationBuckets {
		if s <= le {
			m.buckets[i]++
			break
		}
	}
}

// writeMetrics writes every metric in the Prometheus text format.
func (d *Drover) writeMetrics(w io.Writer) {
	apps := d.appList()
	head := func(name, kind, help string) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, kind)
	}
	head("drover_build_info", "gauge", "The running drover version.")
	fmt.Fprintf(w, "drover_build_info{version=%q} 1\n", version)

	head("drover_requests_total", "counter", "Requests answered by the load balancer, by status code (499: the client went away, 101: upgraded connection).")
	for _, a := range apps {
		a.metrics.mu.Lock()
		codes := make([]int, 0, len(a.metrics.codes))
		for c := range a.metrics.codes {
			codes = append(codes, c)
		}
		sort.Ints(codes)
		for _, c := range codes {
			fmt.Fprintf(w, "drover_requests_total{app=%q,code=\"%d\"} %d\n", a.cfg.Name, c, a.metrics.codes[c])
		}
		a.metrics.mu.Unlock()
	}

	head("drover_request_duration_seconds", "histogram", "Time from receiving a request to finishing its response.")
	for _, a := range apps {
		a.metrics.mu.Lock()
		var cum uint64
		for i, le := range durationBuckets {
			cum += a.metrics.buckets[i]
			fmt.Fprintf(w, "drover_request_duration_seconds_bucket{app=%q,le=\"%g\"} %d\n", a.cfg.Name, le, cum)
		}
		fmt.Fprintf(w, "drover_request_duration_seconds_bucket{app=%q,le=\"+Inf\"} %d\n", a.cfg.Name, a.metrics.count)
		fmt.Fprintf(w, "drover_request_duration_seconds_sum{app=%q} %g\n", a.cfg.Name, a.metrics.sum)
		fmt.Fprintf(w, "drover_request_duration_seconds_count{app=%q} %d\n", a.cfg.Name, a.metrics.count)
		a.metrics.mu.Unlock()
	}

	counter := func(name, help string, get func(*App) uint64) {
		head(name, "counter", help)
		for _, a := range apps {
			fmt.Fprintf(w, "%s{app=%q} %d\n", name, a.cfg.Name, get(a))
		}
	}
	counter("drover_retries_total", "Requests sent to another instance because the first could not be connected to.",
		func(a *App) uint64 { return a.metrics.retries.Load() })
	counter("drover_instance_exits_total", "Times an instance process exited by itself and was restarted.",
		func(a *App) uint64 { return a.metrics.exits.Load() })

	head("drover_rollouts_total", "counter", "Finished rollouts (version switches and rolling restarts), by result.")
	for _, a := range apps {
		fmt.Fprintf(w, "drover_rollouts_total{app=%q,result=\"success\"} %d\n", a.cfg.Name, a.metrics.rolloutsOK.Load())
		fmt.Fprintf(w, "drover_rollouts_total{app=%q,result=\"failed\"} %d\n", a.cfg.Name, a.metrics.rolloutsFail.Load())
	}

	statuses := make([]AppStatus, len(apps))
	for i, a := range apps {
		statuses[i] = a.status()
	}
	gauge := func(name, help string, get func(AppStatus) int) {
		head(name, "gauge", help)
		for _, st := range statuses {
			fmt.Fprintf(w, "%s{app=%q} %d\n", name, st.Name, get(st))
		}
	}
	gauge("drover_instances", "Instances the app is configured to run.", func(st AppStatus) int { return len(st.Instances) })
	gauge("drover_healthy_instances", "Instances that pass their health check and get traffic.", func(st AppStatus) int {
		n := 0
		for _, in := range st.Instances {
			if in.State == "healthy" {
				n++
			}
		}
		return n
	})
	gauge("drover_rollout_in_progress", "1 while a rollout is running.", func(st AppStatus) int {
		if st.Rolling {
			return 1
		}
		return 0
	})

	gauge("drover_env_pending", "1 while a stored environment change has not been applied by a rollout yet.", func(st AppStatus) int {
		if st.EnvPending {
			return 1
		}
		return 0
	})

	head("drover_inflight_requests", "gauge", "Requests open on each instance right now.")
	for _, st := range statuses {
		for _, in := range st.Instances {
			fmt.Fprintf(w, "drover_inflight_requests{app=%q,instance=\"%d\"} %d\n", st.Name, in.Slot, in.Inflight)
		}
	}
	head("drover_version_info", "gauge", "The version each app is on.")
	for _, st := range statuses {
		fmt.Fprintf(w, "drover_version_info{app=%q,version=%q} 1\n", st.Name, st.Version)
	}
}
