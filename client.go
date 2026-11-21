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
	case "app":
		return runApp(args)
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
		if err := need(1, "<app> --url <url> | --image <ref> --path <file or folder> | --git <url> [--ref r] [--subdir d]"); err != nil {
			return err
		}
		fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
		var req releaseRequest
		req.Headers = map[string]string{}
		fs.Func("header", `a header to send with a --url download, as "Name: value" (repeatable)`, func(h string) error {
			k, v, ok := strings.Cut(h, ":")
			if !ok || strings.TrimSpace(k) == "" {
				return fmt.Errorf(`%q is not "Name: value"`, h)
			}
			req.Headers[strings.TrimSpace(k)] = strings.TrimSpace(v)
			return nil
		})
		fs.StringVar(&req.Git, "git", "", "take a folder release from this git repository (folder apps)")
		fs.StringVar(&req.Ref, "ref", "", "branch, tag or commit of the git repository (default: its HEAD)")
		fs.StringVar(&req.Subdir, "subdir", "", "keep only this folder of the archive or repository (folder apps)")
		fs.StringVar(&req.URL, "url", "", "download the binary, or for a folder app the archive, from this URL")
		fs.StringVar(&req.Image, "image", "", "take the binary from this container image")
		fs.StringVar(&req.Path, "path", "", "the binary's path inside the image")
		fs.StringVar(&req.Username, "username", "", "user for a private image or git repository, or basic auth for a --url download")
		fs.StringVar(&req.Password, "password", "", "password or token that goes with --username")
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
		if err := need(2, "<app> <file or folder>"); err != nil {
			return err
		}
		fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
		ver := fs.String("version", defaultVersion(), "name of the new version")
		sha := fs.String("sha256", "", "expected checksum")
		noActivate := fs.Bool("no-activate", false, "store the version without switching to it")
		subdir := fs.String("subdir", "", "keep only this folder of the archive (folder apps)")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		st, err := os.Stat(arg(1))
		if err != nil {
			return err
		}
		var f io.ReadCloser
		if st.IsDir() {
			// A folder is sent as a tar.gz made on the way; a failure while reading it ends the upload.
			pr, pw := io.Pipe()
			go func() { pw.CloseWithError(tarGzDir(arg(1), pw)) }()
			f = pr
		} else if f, err = os.Open(arg(1)); err != nil {
			return err
		}
		defer f.Close()
		q := url.Values{}
		if *subdir != "" {
			q.Set("subdir", *subdir)
		}
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

// runApp implements `drover app add|rm|show|list`.
func runApp(args []string) error {
	usage := errors.New("usage: drover app add <name> --listen <addr> [flags] -- <command> | app rm <name> [--purge] | app show <name> | app list")
	if len(args) == 0 {
		return usage
	}
	switch args[0] {
	case "list", "ls":
		return call(http.MethodGet, "/apps", nil, "")
	case "show":
		if len(args) != 2 {
			return usage
		}
		return call(http.MethodGet, "/apps/"+url.PathEscape(args[1])+"/spec", nil, "")
	case "rm", "remove":
		if len(args) < 2 || strings.HasPrefix(args[1], "-") {
			return usage
		}
		fs := flag.NewFlagSet("app rm", flag.ContinueOnError)
		purge := fs.Bool("purge", false, "also delete the app's versions")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		q := ""
		if *purge {
			q = "?purge=true"
		}
		return call(http.MethodDelete, "/apps/"+url.PathEscape(args[1])+q, nil, "")
	case "add":
		if len(args) < 2 || strings.HasPrefix(args[1], "-") {
			return usage
		}
		spec := appSpec{Name: args[1], Env: map[string]string{}}
		fs := flag.NewFlagSet("app add", flag.ContinueOnError)
		fs.StringVar(&spec.Listen, "listen", "", "public address, e.g. :8081")
		fs.IntVar(&spec.Instances, "instances", 2, "number of instances")
		fs.IntVar(&spec.PortBase, "port-base", 0, "first instance port (default: the first free range from 9000)")
		fs.StringVar(&spec.Health.Path, "health", "", "health check path")
		fs.StringVar(&spec.Ready.Path, "ready", "", "readiness path an instance must pass before it gets traffic")
		fs.StringVar(&spec.Folder, "folder", "", "built-in folder of a folder app ({dir} in the command)")
		fs.StringVar(&spec.Binary, "binary", "", "the file releases replace, when it is not the first word")
		fs.StringVar(&spec.EnvFile, "env-file", "", "KEY=VALUE file read at every instance start")
		fs.StringVar(&spec.Dir, "dir", "", "working directory")
		fs.Func("env", "KEY=VALUE for the app's environment, stored in apps.json (repeatable; secrets belong in --env-file)", func(kv string) error {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				return fmt.Errorf("%q is not KEY=VALUE", kv)
			}
			spec.Env[k] = v
			return nil
		})
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		if fs.NArg() == 0 {
			return errors.New("give the command after --: drover app add <name> --listen :8081 -- <command>")
		}
		spec.Command = fs.Args()
		body, _ := json.Marshal(spec)
		return call(http.MethodPost, "/apps", bytes.NewReader(body), "application/json")
	}
	return usage
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
