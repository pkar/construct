package vcs

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestStamperEnv(t *testing.T) {
	env := map[string]string{"BUILD": "42", "EMPTY": "", "REF": "pr/7"}
	s := &Stamper{Dir: t.TempDir(), LookupEnv: func(k string) (string, bool) { v, ok := env[k]; return v, ok }}
	for in, want := range map[string]string{
		"app:{env.BUILD}":     "app:42",
		"app:x{env.EMPTY}y":   "app:xy",
		"app:{env.REF}":       "app:pr-7",
		"app:{os}-{arch}":     "app:{os}-{arch}",
		"no variables at all": "no variables at all",
	} {
		if got, err := s.Expand(in, true); err != nil || got != want {
			t.Errorf("Expand(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if got, err := s.Expand("{env.REF}", false); err != nil || got != "pr/7" {
		t.Errorf("label Expand = %q, %v; want unsanitised value", got, err)
	}
	for _, bad := range []string{"app:{env.NOPE}", "app:{env.}", "app:{git.commit}", "app:{git.sha}"} {
		if got, err := s.Expand(bad, true); err == nil {
			t.Errorf("Expand(%q) = %q, want error", bad, got)
		}
	}
}

func TestStamperGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	dir := t.TempDir()
	gitCmd(t, dir, "init", "-q", "-b", "feature/login")
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "f")
	gitCmd(t, dir, "commit", "-q", "-m", "one")

	s := &Stamper{Dir: dir}
	got, err := s.Expand("app:{git.branch}-{git.short}", true)
	if err != nil || !strings.HasPrefix(got, "app:feature-login-") || len(got) != len("app:feature-login-")+12 {
		t.Errorf("Expand = %q, %v", got, err)
	}
	if _, err := s.Expand("app:{git.tag}", true); err == nil || !strings.Contains(err.Error(), "no tag points at HEAD") {
		t.Errorf("untagged {git.tag}: err = %v", err)
	}
	m := map[string]string{"rev": "{git.commit}", "plain": "x"}
	if err := s.ExpandAll(m); err != nil || len(m["rev"]) != 40 || m["plain"] != "x" {
		t.Errorf("ExpandAll = %v, %v", m, err)
	}
}
