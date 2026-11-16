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
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"golang.org/x/net/http/httpguts"
)

// releaseRequest asks drover to fetch a binary itself: from a URL, or from a container image through an
// image-file-fetch server.
type releaseRequest struct {
	Version string `json:"version"`
	URL     string `json:"url"`
	Image   string `json:"image"`
	Path    string `json:"path"`
	// Git is a repository to take a folder release from, at Ref (a branch, tag or commit; default HEAD).
	Git string `json:"git"`
	Ref string `json:"ref"`
	// Subdir keeps only this folder of an archive or repository as the release.
	Subdir string `json:"subdir"`
	// Headers are sent with the request for a url source, for downloads that need a token. Like Username
	// and Password they are used for this one request and never stored.
	Headers  map[string]string `json:"headers"`
	Username string            `json:"username"`
	Password string            `json:"password"`
	SHA256   string            `json:"sha256"`
	Activate *bool             `json:"activate"`
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
	g.GET("/apps/:app/env", d.withApp(func(c echo.Context, a *App) error {
		return c.JSON(http.StatusOK, a.envStatus())
	}))
	// Overrides: in memory, lost when drover restarts.
	g.PATCH("/apps/:app/env", d.withApp(func(c echo.Context, a *App) error {
		change, err := readEnvChange(c)
		if err != nil {
			return err
		}
		return envResult(c, a, a.setOverrides(change, false, restartWanted(c)))
	}))
	g.DELETE("/apps/:app/env", d.withApp(func(c echo.Context, a *App) error {
		return envResult(c, a, a.setOverrides(envChange{}, true, restartWanted(c)))
	}))
	// The env file: PUT replaces it with the request body, PATCH edits single variables.
	g.PUT("/apps/:app/env/file", d.withApp(func(c echo.Context, a *App) error {
		body, err := io.ReadAll(io.LimitReader(c.Request().Body, 1<<20+1))
		if err != nil || len(body) > 1<<20 {
			return echo.NewHTTPError(http.StatusBadRequest, "the env file must be readable and at most 1 MiB")
		}
		if _, _, err := parseEnv(body); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		return envResult(c, a, a.writeEnvFile(restartWanted(c), func([]byte) ([]byte, error) { return body, nil }))
	}))
	g.PATCH("/apps/:app/env/file", d.withApp(func(c echo.Context, a *App) error {
		change, err := readEnvChange(c)
		if err != nil {
			return err
		}
		return envResult(c, a, a.writeEnvFile(restartWanted(c), func(current []byte) ([]byte, error) {
			if _, _, err := parseEnv(current); err != nil {
				return nil, fmt.Errorf("the current env_file cannot be edited (replace it with PUT): %w", err)
			}
			return editEnv(current, change), nil
		}))
	}))
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

func restartWanted(c echo.Context) bool { return c.QueryParam("restart") != "false" }

func readEnvChange(c echo.Context) (envChange, error) {
	var change envChange
	if err := json.NewDecoder(c.Request().Body).Decode(&change); err != nil {
		return change, echo.NewHTTPError(http.StatusBadRequest, "the body must be JSON: "+err.Error())
	}
	if err := change.validate(); err != nil {
		return change, echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	return change, nil
}

// envResult answers an environment change with the variable names by source, or the reason it failed.
func envResult(c echo.Context, a *App, err error) error {
	switch {
	case err == nil:
		return c.JSON(http.StatusOK, a.envStatus())
	case errors.Is(err, errBusy), errors.Is(err, errNoEnvFile):
		return echo.NewHTTPError(http.StatusConflict, err.Error())
	case errors.Is(err, errRolledBack):
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	case strings.Contains(err.Error(), "not writable"):
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return echo.NewHTTPError(http.StatusBadRequest, err.Error())
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
	activate := req.Activate == nil || *req.Activate
	if err := checkHeaders(req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	if req.Git != "" {
		if req.URL != "" || req.Image != "" {
			return echo.NewHTTPError(http.StatusBadRequest, "give one of url, image or git")
		}
		return d.storeGit(c, a, req, activate)
	}
	body, source, err := d.open(c.Request().Context(), req, a.cfg.folderMode)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadGateway, err.Error())
	}
	defer body.Close()
	return d.store(c, a, req.Version, source, body, req.SHA256, req.Subdir, activate)
}

// storeGit makes a folder release from a commit of a git repository, or from one folder of it.
func (d *Drover) storeGit(c echo.Context, a *App, req releaseRequest, activate bool) error {
	rel, err := a.store.addFolder(req.Version, d.cfg.MaxBinaryBytes, func(tree string) (string, error) {
		clone := tree + ".clone"
		defer os.RemoveAll(clone)
		commit, err := gitCheckout(c.Request().Context(), req, clone)
		if err != nil {
			return "", err
		}
		sub := cleanRel(req.Subdir)
		src := filepath.Join(clone, filepath.FromSlash(sub))
		if st, err := os.Lstat(src); err != nil || !st.IsDir() {
			return "", fmt.Errorf("the repository has no folder %q at %s", req.Subdir, commit[:min(12, len(commit))])
		}
		source := "git:" + req.Git + "@" + commit
		if sub != "" {
			source += ":" + sub
		}
		return source, os.Rename(src, tree)
	})
	switch {
	case errors.Is(err, errGitRemote):
		return echo.NewHTTPError(http.StatusBadGateway, err.Error())
	case err != nil:
		return httpError(err)
	case !activate:
		return c.JSON(http.StatusCreated, rel)
	}
	return d.activate(c, a, rel.Version)
}

// uploadRelease stores the request body as a version: `curl -T app-v2 .../apps/web/releases/v2`. For a
// folder app the body is an archive, and ?subdir= keeps one folder of it.
func (d *Drover) uploadRelease(c echo.Context, a *App) error {
	activate := c.QueryParam("activate") != "false"
	return d.store(c, a, c.Param("version"), "upload", c.Request().Body, c.QueryParam("sha256"), c.QueryParam("subdir"), activate)
}

func (d *Drover) store(c echo.Context, a *App, version, source string, r io.Reader, sha, subdir string, activate bool) error {
	rel, err := a.store.add(version, source, r, sha, d.cfg.MaxBinaryBytes, subdir)
	if err != nil {
		return httpError(err)
	}
	if !activate {
		return c.JSON(http.StatusCreated, rel)
	}
	return d.activate(c, a, rel.Version)
}

// Headers a caller may not set: they describe the request itself.
var fixedHeaders = map[string]bool{"Host": true, "Content-Length": true, "Transfer-Encoding": true, "Connection": true}

// checkHeaders refuses headers on sources that have no use for them, and names or values that are not
// valid in an HTTP request.
func checkHeaders(req releaseRequest) error {
	if len(req.Headers) == 0 {
		return nil
	}
	if req.URL == "" {
		return errors.New("headers are only for url sources (image and git take username and password)")
	}
	for k, v := range req.Headers {
		if !httpguts.ValidHeaderFieldName(k) || fixedHeaders[http.CanonicalHeaderKey(k)] {
			return fmt.Errorf("header %q cannot be set", k)
		}
		if !httpguts.ValidHeaderFieldValue(v) {
			return fmt.Errorf("the value of header %q is not valid", k)
		}
	}
	return nil
}

// keepCredentialsOnHost is a redirect policy: when a download is redirected to another host (or from https
// to http), the caller's headers and the Authorization header are dropped, so a token is never sent to a
// server it was not meant for. Go itself only drops Authorization, and only for unrelated domains.
func keepCredentialsOnHost(headers map[string]string) func(*http.Request, []*http.Request) error {
	return func(next *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		first := via[0].URL
		if next.URL.Host != first.Host || (first.Scheme == "https" && next.URL.Scheme != "https") {
			next.Header.Del("Authorization")
			for k := range headers {
				next.Header.Del(k)
			}
		}
		return nil
	}
}

// open returns what the request names as a stream: a binary, or for a folder app an archive. An image
// folder is asked from image-file-fetch as a tar.
func (d *Drover) open(ctx context.Context, req releaseRequest, folder bool) (io.ReadCloser, string, error) {
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
		for k, v := range req.Headers {
			hreq.Header.Set(k, v)
		}
		if (req.Username != "" || req.Password != "") && hreq.Header.Get("Authorization") == "" {
			hreq.SetBasicAuth(req.Username, req.Password)
		}
		// Credentials in the URL are not written to meta.json.
		source = u.Redacted()
	case req.Image != "":
		if d.cfg.ImageFetchURL == "" {
			return nil, "", errors.New("image sources need image_fetch_url (or DROVER_IMAGE_FETCH_URL) to point at an image-file-fetch server")
		}
		if req.Path == "" {
			return nil, "", errors.New("image sources need path: the place of the file (or, for a folder app, the folder) inside the image")
		}
		fields := map[string]string{"image": req.Image, "path": req.Path,
			"username": req.Username, "password": req.Password}
		if folder {
			fields["archive"] = "tar"
		}
		payload, _ := json.Marshal(fields)
		hreq, _ = http.NewRequestWithContext(ctx, http.MethodPost, d.cfg.ImageFetchURL+"/download", bytes.NewReader(payload))
		hreq.Header.Set("Content-Type", "application/json")
		source = req.Image + ":" + req.Path
	default:
		return nil, "", errors.New("give a url, an image and a path, or (for a folder app) a git repository")
	}
	client := *d.fetcher
	client.CheckRedirect = keepCredentialsOnHost(req.Headers)
	resp, err := client.Do(hreq)
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
