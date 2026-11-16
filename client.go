package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// runClient implements the subcommands that talk to a running drover, for images that have no curl.
func runClient(cmd string, args []string) error {
	arg := func(i int) string {
		if i < len(args) {
			return args[i]
		}
		return ""
	}
	need := func(n int, what string) error {
		if len(args) < n || strings.HasPrefix(arg(n-1), "-") {
			return fmt.Errorf("usage: drover %s %s", cmd, what)
		}
		return nil
	}
	app := url.PathEscape(arg(0))
	switch cmd {
	case "status":
		if arg(0) == "" {
			return call(http.MethodGet, "/apps", nil, "")
		}
		return call(http.MethodGet, "/apps/"+app, nil, "")
	case "env":
		return runEnv(args)
	case "logs":
		if err := need(1, "<app> [-f] [--tail n] [--instance n]"); err != nil {
			return err
		}
		fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
		follow := fs.Bool("f", false, "keep printing new lines")
		tail := fs.Int("tail", 200, "how many recent lines to start with")
		instance := fs.Int("instance", -1, "only this instance")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		q := url.Values{"tail": {strconv.Itoa(*tail)}}
		if *follow {
			q.Set("follow", "true")
		}
		if *instance >= 0 {
			q.Set("instance", strconv.Itoa(*instance))
		}
		return call(http.MethodGet, "/apps/"+app+"/logs?"+q.Encode(), nil, "")
	case "metrics":
		return call(http.MethodGet, "/metrics", nil, "")
	case "releases":
		if err := need(1, "<app>"); err != nil {
			return err
		}
		return call(http.MethodGet, "/apps/"+app+"/releases", nil, "")
	case "restart", "rollback":
		if err := need(1, "<app>"); err != nil {
			return err
		}
		return call(http.MethodPost, "/apps/"+app+"/"+cmd, nil, "")
	case "activate":
		if err := need(2, "<app> <version>"); err != nil {
			return err
		}
		return call(http.MethodPost, "/apps/"+app+"/releases/"+url.PathEscape(arg(1))+"/activate", nil, "")
	case "rm":
		if err := need(2, "<app> <version>"); err != nil {
			return err
		}
		return call(http.MethodDelete, "/apps/"+app+"/releases/"+url.PathEscape(arg(1)), nil, "")
	case "fetch":
		if err := need(1, "<app> --url <url> | --image <ref> --path <file>"); err != nil {
			return err
		}
		fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
		var req releaseRequest
		fs.StringVar(&req.URL, "url", "", "download the binary from this URL")
		fs.StringVar(&req.Image, "image", "", "take the binary from this container image")
		fs.StringVar(&req.Path, "path", "", "the binary's path inside the image")
		fs.StringVar(&req.Username, "username", "", "registry user")
		fs.StringVar(&req.Password, "password", "", "registry password")
		fs.StringVar(&req.Version, "version", "", "name of the new version (default: a timestamp)")
		fs.StringVar(&req.SHA256, "sha256", "", "expected checksum")
		noActivate := fs.Bool("no-activate", false, "store the version without switching to it")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *noActivate {
			f := false
			req.Activate = &f
		}
		body, _ := json.Marshal(req)
		return call(http.MethodPost, "/apps/"+app+"/releases", bytes.NewReader(body), "application/json")
	case "upload":
		if err := need(2, "<app> <file>"); err != nil {
			return err
		}
		fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
		ver := fs.String("version", defaultVersion(), "name of the new version")
		sha := fs.String("sha256", "", "expected checksum")
		noActivate := fs.Bool("no-activate", false, "store the version without switching to it")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		f, err := os.Open(arg(1))
		if err != nil {
			return err
		}
		defer f.Close()
		q := url.Values{}
		if *sha != "" {
			q.Set("sha256", *sha)
		}
		if *noActivate {
			q.Set("activate", "false")
		}
		return call(http.MethodPut, "/apps/"+app+"/releases/"+url.PathEscape(*ver)+"?"+q.Encode(), f, "application/octet-stream")
	}
	return fmt.Errorf("unknown command %q (try `drover help`)", cmd)
}

// runEnv implements `drover env ...`. Flags may come anywhere after the subcommand.
func runEnv(all []string) error {
	var args []string
	file, restart := false, true
	for _, a := range all {
		switch a {
		case "--file", "-file":
			file = true
		case "--no-restart", "-no-restart":
			restart = false
		default:
			args = append(args, a)
		}
	}
	usage := errors.New("usage: drover env <app> | env set <app> KEY=VALUE... [--file] | env unset <app> KEY... [--file] | env clear <app> | env put <app> <file>   (all but the first take --no-restart)")
	if len(args) == 0 {
		return usage
	}
	sub := ""
	if len(args) >= 2 {
		switch args[0] {
		case "set", "unset", "clear", "put":
			sub, args = args[0], args[1:]
		}
	}
	path := "/apps/" + url.PathEscape(args[0]) + "/env"
	q := ""
	if !restart {
		q = "?restart=false"
	}
	switch sub {
	case "":
		if len(args) != 1 {
			return usage
		}
		return call(http.MethodGet, path, nil, "")
	case "clear":
		return call(http.MethodDelete, path+q, nil, "")
	case "put":
		if len(args) != 2 {
			return usage
		}
		f, err := os.Open(args[1])
		if err != nil {
			return err
		}
		defer f.Close()
		return call(http.MethodPut, path+"/file"+q, f, "text/plain")
	}
	if len(args) < 2 {
		return usage
	}
	change := envChange{Set: map[string]string{}}
	for _, kv := range args[1:] {
		if sub == "unset" {
			change.Unset = append(change.Unset, kv)
			continue
		}
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return fmt.Errorf("%q is not KEY=VALUE", kv)
		}
		change.Set[k] = v
	}
	if file {
		path += "/file"
	}
	body, _ := json.Marshal(change)
	return call(http.MethodPatch, path+q, bytes.NewReader(body), "application/json")
}

func call(method, path string, body io.Reader, contentType string) error {
	base := strings.TrimRight(os.Getenv("DROVER_ADMIN"), "/")
	if base == "" {
		base = "http://127.0.0.1:9100"
	}
	req, err := http.NewRequest(method, base+path, body)
	if err != nil {
		return err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if t := os.Getenv("DROVER_TOKEN"); t != "" {
		// Not Authorization: that stays free for a proxy's login, e.g. DROVER_ADMIN=https://user:pass@host.
		req.Header.Set("X-Drover-Token", t)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 300 && strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain") {
		// Logs and metrics: print as they arrive, so following works.
		_, err := io.Copy(os.Stdout, resp.Body)
		return err
	}
	raw, _ := io.ReadAll(resp.Body)
	var pretty bytes.Buffer
	if json.Indent(&pretty, raw, "", "  ") == nil {
		raw = append(pretty.Bytes(), '\n')
	}
	if resp.StatusCode >= 300 {
		return errors.New(resp.Status + ": " + strings.TrimSpace(string(raw)))
	}
	os.Stdout.Write(raw)
	return nil
}
