package vcs

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestPublicURL(t *testing.T) {
	for in, want := range map[string]string{
		"git@github.com:org/repo.git":                  "https://github.com/org/repo",
		"github.com:org/repo":                          "https://github.com/org/repo",
		"https://user:s3cret@github.com/org/repo.git":  "https://github.com/org/repo",
		"https://token@git.example.com:8443/org/repo/": "https://git.example.com:8443/org/repo",
		"ssh://git@git.example.com:2222/org/repo.git":  "https://git.example.com/org/repo",
		"/srv/git/repo.git":                            "",
		"file:///srv/git/repo.git":                     "",
		"../repo":                                      "",
		"C:\\repos\\x":                                 "",
		"  git@gitlab.com:group/sub/repo.git\n":        "https://gitlab.com/group/sub/repo",
	} {
		if got := PublicURL(in); got != want {
			t.Errorf("PublicURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func gitCmd(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestRead(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	dir := t.TempDir()
	if _, err := Read(dir); err != ErrNotRepo {
		t.Fatalf("Read(non-repo) err = %v, want ErrNotRepo", err)
	}

	gitCmd(t, dir, "init", "-q", "-b", "main")
	if _, err := Read(dir); err != ErrNotRepo {
		t.Fatalf("Read(empty repo) err = %v, want ErrNotRepo", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "f")
	gitCmd(t, dir, "commit", "-q", "-m", "one")
	gitCmd(t, dir, "tag", "v1.2.0")
	gitCmd(t, dir, "tag", "v1.10.0")
	gitCmd(t, dir, "remote", "add", "origin", "https://me:pw@example.com/org/app.git")

	info, err := Read(filepath.Join(dir))
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Commit) != 40 || len(info.Short) != 12 || info.Commit[:12] != info.Short {
		t.Errorf("commit %q short %q", info.Commit, info.Short)
	}
	if info.Branch != "main" || info.Tag != "v1.10.0" || info.Remote != "https://example.com/org/app" || info.Dirty {
		t.Errorf("info = %+v", info)
	}

	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("2"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "checkout", "-q", "--detach")
	if info, err = Read(dir); err != nil || !info.Dirty || info.Branch != "" {
		t.Errorf("dirty detached info = %+v, %v", info, err)
	}
}
