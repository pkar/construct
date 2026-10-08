// Package vcs reads the Git state of a source tree for image annotations
// and tag stamping.
package vcs

import (
	"bytes"
	"errors"
	"net/url"
	"os/exec"
	"strings"
)

// Info is the Git state of a working tree.
type Info struct {
	Commit string // full commit hash
	Short  string // abbreviated commit hash
	Branch string // current branch, empty when detached
	Tag    string // tag pointing at HEAD, empty if none
	Remote string // origin URL without credentials, empty if none
	Dirty  bool   // uncommitted changes to tracked files
}

// ErrNotRepo means dir is not inside a Git working tree, or Git is not
// installed.
var ErrNotRepo = errors.New("not a git repository")

// Read returns the Git state of the working tree containing dir.
func Read(dir string) (Info, error) {
	commit, err := git(dir, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return Info{}, ErrNotRepo
	}
	info := Info{Commit: commit}
	info.Short, _ = git(dir, "rev-parse", "--short=12", "HEAD")
	info.Branch, _ = git(dir, "symbolic-ref", "--quiet", "--short", "HEAD")
	if tags, err := git(dir, "tag", "--points-at", "HEAD", "--sort=-version:refname"); err == nil && tags != "" {
		info.Tag, _, _ = strings.Cut(tags, "\n")
	}
	if remote, err := git(dir, "config", "--get", "remote.origin.url"); err == nil {
		info.Remote = PublicURL(remote)
	}
	if status, err := git(dir, "status", "--porcelain", "--untracked-files=no"); err == nil {
		info.Dirty = status != ""
	}
	return info, nil
}

// PublicURL turns a Git remote into a browsable URL with no credentials:
// git@github.com:org/repo.git becomes https://github.com/org/repo. It
// returns "" for local paths and anything it does not understand.
func PublicURL(remote string) string {
	remote = strings.TrimSpace(remote)
	if strings.ContainsAny(remote, "\\") {
		return ""
	}
	if strings.Contains(remote, "://") {
		u, err := url.Parse(remote)
		if err != nil || u.Host == "" {
			return ""
		}
		switch u.Scheme {
		case "http", "https", "ssh", "git":
		default:
			return ""
		}
		host := u.Hostname()
		if u.Scheme == "http" || u.Scheme == "https" {
			host = u.Host
		}
		return "https://" + host + trimGit(u.Path)
	}
	// scp-like syntax: [user@]host:path
	if at := strings.Index(remote, "@"); at >= 0 {
		remote = remote[at+1:]
	}
	host, p, ok := strings.Cut(remote, ":")
	if !ok || host == "" || p == "" || strings.ContainsAny(host, "/\\") {
		return ""
	}
	return "https://" + host + "/" + trimGit(strings.TrimPrefix(p, "/"))
}

func trimGit(p string) string {
	return strings.TrimSuffix(strings.TrimSuffix(p, "/"), ".git")
}

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return strings.TrimSpace(out.String()), nil
}
