package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// errGitRemote marks a failure to get the commit from the repository, as opposed to a bad request.
var errGitRemote = errors.New("could not fetch from the git repository")

// gitSchemes are the URL schemes a git source may use. Tests add "file".
var gitSchemes = map[string]bool{"https": true, "http": true}

// A ref is a branch, a tag or a commit. It never starts with "-", so it cannot be read as an option.
var gitRefRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._/@^~-]{0,199}$`)

// gitCheckout puts the files of one commit of a repository into dest (which must not exist) and returns
// the commit. It fetches only that commit, without history, and runs the git command, which therefore
// has to be in the container.
func gitCheckout(ctx context.Context, req releaseRequest, dest string) (string, error) {
	u, err := url.Parse(req.Git)
	if err != nil || !gitSchemes[u.Scheme] || (u.Host == "" && u.Scheme != "file") {
		return "", errors.New("git must be an http or https URL of a repository")
	}
	if u.User != nil {
		return "", errors.New("put git credentials in username and password, not in the URL")
	}
	ref := req.Ref
	if ref == "" {
		ref = "HEAD"
	}
	if !gitRefRe.MatchString(ref) {
		return "", fmt.Errorf("invalid ref %q", req.Ref)
	}
	if _, err := exec.LookPath("git"); err != nil {
		return "", errors.New("git sources need the git command in the container, and it is not installed")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()

	env := append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if req.Username != "" || req.Password != "" {
		// Passed through the environment, so the credentials are not in the process list.
		basic := base64.StdEncoding.EncodeToString([]byte(req.Username + ":" + req.Password))
		env = append(env, "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.extraHeader",
			"GIT_CONFIG_VALUE_0=Authorization: Basic "+basic)
	}
	git := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			msg := strings.TrimSpace(string(out))
			if len(msg) > 600 {
				msg = msg[len(msg)-600:]
			}
			step := args[0]
			if step == "-C" {
				step = args[2]
			}
			return "", fmt.Errorf("%w: git %s: %v: %s", errGitRemote, step, err, msg)
		}
		return strings.TrimSpace(string(out)), nil
	}
	if _, err := git("init", "-q", dest); err != nil {
		return "", err
	}
	if _, err := git("-C", dest, "fetch", "-q", "--depth", "1", "--no-tags", "--", req.Git, ref); err != nil {
		return "", err
	}
	if _, err := git("-C", dest, "checkout", "-q", "FETCH_HEAD"); err != nil {
		return "", err
	}
	commit, err := git("-C", dest, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	return commit, os.RemoveAll(filepath.Join(dest, ".git"))
}
