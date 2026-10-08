package vcs

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// Stamper expands {git.commit}, {git.short}, {git.branch}, {git.tag}, and
// {env.NAME} in tags, labels, and annotations. Git is read once, on first
// use, from the working tree containing Dir.
type Stamper struct {
	Dir string
	// LookupEnv defaults to os.LookupEnv.
	LookupEnv func(string) (string, bool)

	read bool
	info Info
	err  error
}

var stampVar = regexp.MustCompile(`\{(git|env)\.([A-Za-z0-9_]*)\}`)

// tagUnsafe matches characters that cannot appear in an image tag.
var tagUnsafe = regexp.MustCompile(`[^A-Za-z0-9_.-]`)

// Expand replaces stamp variables in s. With forTag set, substituted values
// have characters that are not allowed in image tags replaced with "-", so
// branch feature/login becomes feature-login. Unknown variables, a missing
// repository, an untagged HEAD, and unset environment variables are
// errors, so a tag never silently loses a part.
func (s *Stamper) Expand(v string, forTag bool) (string, error) {
	var firstErr error
	out := stampVar.ReplaceAllStringFunc(v, func(m string) string {
		sub := stampVar.FindStringSubmatch(m)
		val, err := s.lookup(sub[1], sub[2])
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("%q: %s: %w", v, m, err)
			}
			return m
		}
		if forTag {
			val = tagUnsafe.ReplaceAllString(val, "-")
		}
		return val
	})
	return out, firstErr
}

// ExpandAll expands every value of m in place.
func (s *Stamper) ExpandAll(m map[string]string) error {
	for k, v := range m {
		x, err := s.Expand(v, false)
		if err != nil {
			return err
		}
		m[k] = x
	}
	return nil
}

func (s *Stamper) lookup(ns, key string) (string, error) {
	if ns == "env" {
		lookup := s.LookupEnv
		if lookup == nil {
			lookup = os.LookupEnv
		}
		if key == "" {
			return "", fmt.Errorf("want {env.NAME}")
		}
		val, ok := lookup(key)
		if !ok {
			return "", fmt.Errorf("environment variable %s is not set", key)
		}
		return val, nil
	}
	if !s.read {
		dir := s.Dir
		if dir == "" {
			dir = "."
		}
		s.info, s.err = Read(dir)
		s.read = true
	}
	if s.err != nil {
		return "", s.err
	}
	var val, missing string
	switch key {
	case "commit":
		val = s.info.Commit
	case "short":
		val = s.info.Short
	case "branch":
		val, missing = s.info.Branch, "HEAD is detached"
	case "tag":
		val, missing = s.info.Tag, "no tag points at HEAD"
	default:
		return "", fmt.Errorf("unknown variable; want one of commit, short, branch, tag")
	}
	if val == "" {
		return "", fmt.Errorf("%s", missing)
	}
	return val, nil
}

// HasVars reports whether s contains stamp variables.
func HasVars(s string) bool { return strings.Contains(s, "{git.") || strings.Contains(s, "{env.") }
