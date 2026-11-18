package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/labstack/echo/v4"
)

// Drover is the running program: the apps, one public listener per app, and the admin API.
type Drover struct {
	cfg         *Config
	appsMu      sync.RWMutex
	apps        []*App
	manageMu    sync.Mutex         // one add or remove at a time; guards specs
	specs       map[string]appSpec // apps added through the API, as recorded in apps.json
	servers     []*echo.Echo       // admin API and metrics
	adminAddr   net.Addr
	metricsAddr net.Addr
	fetcher     *http.Client
	done        chan struct{} // closed when drover stops, to end log followers
	stopOnce    sync.Once
}

func newDrover(cfg *Config) (*Drover, error) {
	if err := cfg.normalize(); err != nil {
		return nil, err
	}
	d := &Drover{cfg: cfg, fetcher: &http.Client{Timeout: 30 * time.Minute}, done: make(chan struct{}),
		specs: map[string]appSpec{}}
	for _, ac := range cfg.Apps {
		a, err := newApp(ac, cfg.Data, cfg.LogLines)
		if err != nil {
			return nil, fmt.Errorf("app %q: %w", ac.Name, err)
		}
		a.origin = originConfig
		d.apps = append(d.apps, a)
	}
	if err := d.loadManaged(); err != nil {
		return nil, err
	}
	return d, nil
}

// appList returns the apps as they are now. Apps can be added and removed while drover runs.
func (d *Drover) appList() []*App {
	d.appsMu.RLock()
	defer d.appsMu.RUnlock()
	return append([]*App(nil), d.apps...)
}

func (d *Drover) findApp(name string) *App {
	for _, a := range d.appList() {
		if a.cfg.Name == name {
			return a
		}
	}
	return nil
}

func newEcho() *echo.Echo {
	e := echo.New()
	e.HideBanner, e.HidePort = true, true
	return e
}

// listenApp opens an app's public port and serves its load balancer on it.
func (d *Drover) listenApp(a *App) error {
	ln, err := net.Listen("tcp", a.cfg.Listen)
	if err != nil {
		return fmt.Errorf("app %q: %w", a.cfg.Name, err)
	}
	a.addr = ln.Addr()
	e := newEcho()
	// Every request, whatever its method or path, goes to the load balancer: no routing in front of it.
	e.Pre(func(echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			began := time.Now()
			a.ServeHTTP(c.Response(), c.Request())
			code := c.Response().Status
			if !c.Response().Committed {
				// Nothing was written through the response: the connection was upgraded and handed over,
				// or the client went away first.
				code = 499
				if c.Request().Header.Get("Upgrade") != "" {
					code = 101
				}
			}
			a.metrics.observe(code, time.Since(began))
			return nil
		}
	})
	a.server = e
	serve(e, ln)
	log.Printf("[%s] load balancer on %s, %d instance(s), version %s", a.cfg.Name, ln.Addr(), a.cfg.Instances, a.version)
	return nil
}

// start binds every port first, so a port that is taken fails the start before any process is launched.
// An app from the API whose port is taken is left out instead, so it cannot keep the others from running.
func (d *Drover) start() error {
	for _, a := range d.appList() {
		if err := d.listenApp(a); err != nil {
			if a.origin == originAPI {
				log.Printf("[drover] %v: app from the API not started", err)
				d.appsMu.Lock()
				for i, x := range d.apps {
					if x == a {
						d.apps = append(d.apps[:i:i], d.apps[i+1:]...)
						break
					}
				}
				d.appsMu.Unlock()
				continue
			}
			d.stop()
			return err
		}
	}
	ln, err := net.Listen("tcp", d.cfg.Admin.Listen)
	if err != nil {
		d.stop()
		return fmt.Errorf("admin: %w", err)
	}
	d.adminAddr = ln.Addr()
	e := newEcho()
	d.adminRoutes(e)
	d.servers = append(d.servers, e)
	serve(e, ln)
	log.Printf("[drover] admin API on %s (token %s, apps through the API %s), data in %s", ln.Addr(),
		map[bool]string{true: "required", false: "not set"}[d.cfg.Admin.Token != ""],
		map[bool]string{true: "on", false: "off"}[d.cfg.Admin.ManageApps], d.cfg.Data)

	if d.cfg.Metrics.Listen != "" {
		ln, err := net.Listen("tcp", d.cfg.Metrics.Listen)
		if err != nil {
			d.stop()
			return fmt.Errorf("metrics: %w", err)
		}
		d.metricsAddr = ln.Addr()
		e := newEcho()
		e.GET("/metrics", d.metricsHandler)
		d.servers = append(d.servers, e)
		serve(e, ln)
		log.Printf("[drover] metrics on %s", ln.Addr())
	}

	for _, a := range d.appList() {
		a.start()
	}
	return nil
}

func serve(e *echo.Echo, ln net.Listener) {
	e.Listener = ln
	go func() {
		if err := e.Start(""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("[drover] listener %s: %v", ln.Addr(), err)
		}
	}()
}

// stop closes the listeners, giving open requests a moment to finish, then stops every instance.
func (d *Drover) stop() {
	d.stopOnce.Do(func() { close(d.done) })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	apps := d.appList()
	for _, e := range d.servers {
		_ = e.Shutdown(ctx)
	}
	for _, a := range apps {
		if a.server != nil {
			_ = a.server.Shutdown(ctx)
		}
	}
	for _, a := range apps {
		a.stop()
	}
}
