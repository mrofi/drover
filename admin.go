package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
)

// releaseRequest asks drover to fetch a binary itself: from a URL, or from a container image through an
// image-file-fetch server.
type releaseRequest struct {
	Version  string `json:"version"`
	URL      string `json:"url"`
	Image    string `json:"image"`
	Path     string `json:"path"`
	Username string `json:"username"`
	Password string `json:"password"`
	SHA256   string `json:"sha256"`
	Activate *bool  `json:"activate"`
}

func (d *Drover) adminRoutes(e *echo.Echo) {
	e.GET("/healthz", func(c echo.Context) error { return c.String(http.StatusOK, "ok\n") })

	g := e.Group("", d.auth)
	g.GET("/apps", func(c echo.Context) error {
		out := []AppStatus{}
		for _, a := range d.apps {
			out = append(out, a.status())
		}
		return c.JSON(http.StatusOK, out)
	})
	g.GET("/apps/:app", d.withApp(func(c echo.Context, a *App) error {
		return c.JSON(http.StatusOK, a.status())
	}))
	g.GET("/metrics", d.metricsHandler)
	g.GET("/apps/:app/logs", d.withApp(d.logsHandler))
	g.POST("/apps/:app/restart", d.withApp(func(c echo.Context, a *App) error {
		current, _ := a.versions()
		return d.activate(c, a, current)
	}))
	g.POST("/apps/:app/rollback", d.withApp(func(c echo.Context, a *App) error {
		_, previous := a.versions()
		if previous == "" {
			return echo.NewHTTPError(http.StatusConflict, "there is no previous version to go back to")
		}
		return d.activate(c, a, previous)
	}))
	g.GET("/apps/:app/releases", d.withApp(func(c echo.Context, a *App) error {
		current, _ := a.versions()
		return c.JSON(http.StatusOK, a.store.list(current))
	}))
	g.POST("/apps/:app/releases", d.withApp(d.fetchRelease))
	g.PUT("/apps/:app/releases/:version", d.withApp(d.uploadRelease))
	g.POST("/apps/:app/releases/:version/activate", d.withApp(func(c echo.Context, a *App) error {
		return d.activate(c, a, c.Param("version"))
	}))
	g.DELETE("/apps/:app/releases/:version", d.withApp(func(c echo.Context, a *App) error {
		version := c.Param("version")
		current, _ := a.versions()
		if version == versionBuiltin || version == current {
			return echo.NewHTTPError(http.StatusConflict, "the built-in and the current version cannot be deleted")
		}
		if err := a.store.remove(version); err != nil {
			return httpError(err)
		}
		a.forgetPrevious(version)
		return c.NoContent(http.StatusNoContent)
	}))
}

func (d *Drover) metricsHandler(c echo.Context) error {
	c.Response().Header().Set(echo.HeaderContentType, "text/plain; version=0.0.4; charset=utf-8")
	d.writeMetrics(c.Response())
	return nil
}

// logsHandler returns the recent lines of an app as plain text: ?tail=N for the last N (default 200),
// ?instance=N for one instance, ?follow=true to keep the response open and send new lines as they come.
func (d *Drover) logsHandler(c echo.Context, a *App) error {
	slot, tail := -1, 200
	if v := c.QueryParam("instance"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n >= a.cfg.Instances {
			return echo.NewHTTPError(http.StatusBadRequest, "instance must be a number from 0 to "+strconv.Itoa(a.cfg.Instances-1))
		}
		slot = n
	}
	if v := c.QueryParam("tail"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return echo.NewHTTPError(http.StatusBadRequest, "tail must be a number")
		}
		tail = n
	}
	entries, seq, changed := a.logs.read(0, slot)
	if len(entries) > tail {
		entries = entries[len(entries)-tail:]
	}
	res := c.Response()
	res.Header().Set(echo.HeaderContentType, "text/plain; charset=utf-8")
	res.WriteHeader(http.StatusOK)
	writeLogs(res, a.cfg.Name, entries)
	if c.QueryParam("follow") != "true" {
		return nil
	}
	for {
		res.Flush()
		select {
		case <-c.Request().Context().Done():
			return nil
		case <-d.done:
			return nil
		case <-changed:
		}
		entries, seq, changed = a.logs.read(seq, slot)
		writeLogs(res, a.cfg.Name, entries)
	}
}

func (d *Drover) auth(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		if want := d.cfg.Admin.Token; want != "" {
			// X-Drover-Token leaves Authorization free for a proxy in front that has its own login.
			got := c.Request().Header.Get("X-Drover-Token")
			if got == "" {
				got = strings.TrimPrefix(c.Request().Header.Get("Authorization"), "Bearer ")
			}
			if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
				return echo.NewHTTPError(http.StatusUnauthorized, "missing or wrong token")
			}
		}
		return next(c)
	}
}

func (d *Drover) withApp(h func(echo.Context, *App) error) echo.HandlerFunc {
	return func(c echo.Context) error {
		for _, a := range d.apps {
			if a.cfg.Name == c.Param("app") {
				return h(c, a)
			}
		}
		return echo.NewHTTPError(http.StatusNotFound, "no such app")
	}
}

func httpError(err error) error {
	switch {
	case errors.Is(err, errBusy), errors.Is(err, errExists):
		return echo.NewHTTPError(http.StatusConflict, err.Error())
	case errors.Is(err, errNotFound):
		return echo.NewHTTPError(http.StatusNotFound, err.Error())
	}
	return echo.NewHTTPError(http.StatusBadRequest, err.Error())
}

// activate rolls the app to version and reports the result. The request returns when the rollout is over.
func (d *Drover) activate(c echo.Context, a *App, version string) error {
	if err := a.rollout(version); err != nil {
		if errors.Is(err, errBusy) || errors.Is(err, errNotFound) {
			return httpError(err)
		}
		return echo.NewHTTPError(http.StatusInternalServerError,
			fmt.Sprintf("rollout failed, still on the version from before: %v", err))
	}
	return c.JSON(http.StatusOK, a.status())
}

func defaultVersion() string { return time.Now().UTC().Format("20060102-150405") }

// fetchRelease downloads a binary from the source in the request, stores it and (by default) activates it.
func (d *Drover) fetchRelease(c echo.Context, a *App) error {
	var req releaseRequest
	if err := json.NewDecoder(c.Request().Body).Decode(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "the body must be JSON: "+err.Error())
	}
	if req.Version == "" {
		req.Version = defaultVersion()
	}
	body, source, err := d.open(c.Request().Context(), req)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadGateway, err.Error())
	}
	defer body.Close()
	return d.store(c, a, req.Version, source, body, req.SHA256, req.Activate == nil || *req.Activate)
}

// uploadRelease stores the request body as a binary: `curl -T app-v2 .../apps/web/releases/v2`.
func (d *Drover) uploadRelease(c echo.Context, a *App) error {
	activate := c.QueryParam("activate") != "false"
	return d.store(c, a, c.Param("version"), "upload", c.Request().Body, c.QueryParam("sha256"), activate)
}

func (d *Drover) store(c echo.Context, a *App, version, source string, r io.Reader, sha string, activate bool) error {
	rel, err := a.store.add(version, source, r, sha, d.cfg.MaxBinaryBytes)
	if err != nil {
		return httpError(err)
	}
	if !activate {
		return c.JSON(http.StatusCreated, rel)
	}
	return d.activate(c, a, rel.Version)
}

// open returns the binary named by the request as a stream.
func (d *Drover) open(ctx context.Context, req releaseRequest) (io.ReadCloser, string, error) {
	var hreq *http.Request
	var source string
	switch {
	case req.URL != "" && req.Image != "":
		return nil, "", errors.New("give either url or image, not both")
	case req.URL != "":
		u, err := url.Parse(req.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, "", errors.New("url must be an http or https URL")
		}
		hreq, _ = http.NewRequestWithContext(ctx, http.MethodGet, req.URL, nil)
		// Credentials in the URL are not written to meta.json.
		source = u.Redacted()
	case req.Image != "":
		if d.cfg.ImageFetchURL == "" {
			return nil, "", errors.New("image sources need image_fetch_url (or DROVER_IMAGE_FETCH_URL) to point at an image-file-fetch server")
		}
		if req.Path == "" {
			return nil, "", errors.New("image sources need path: the file's place inside the image")
		}
		payload, _ := json.Marshal(map[string]string{"image": req.Image, "path": req.Path,
			"username": req.Username, "password": req.Password})
		hreq, _ = http.NewRequestWithContext(ctx, http.MethodPost, d.cfg.ImageFetchURL+"/download", bytes.NewReader(payload))
		hreq.Header.Set("Content-Type", "application/json")
		source = req.Image + ":" + req.Path
	default:
		return nil, "", errors.New("give a url, or an image and a path")
	}
	resp, err := d.fetcher.Do(hreq)
	if err != nil {
		return nil, "", fmt.Errorf("fetching %s: %w", source, err)
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		return nil, "", fmt.Errorf("fetching %s: %s %s", source, resp.Status, strings.TrimSpace(string(msg)))
	}
	return resp.Body, source, nil
}
