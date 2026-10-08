// Package check runs structure tests against a built image without a
// container runtime: assertions about files in its flattened filesystem
// and about its config.
package check

import (
	"archive/tar"
	"fmt"
	"io"
	"path"
	"regexp"
	"slices"
	"strings"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"gopkg.in/yaml.v3"

	"github.com/pkar/construct/internal/image"
)

// maxContains bounds how much of a file a contains check reads.
const maxContains = 64 << 20

// Check is one assertion. Exactly one of File, Absent, or Config is set.
type Check struct {
	Name string `yaml:"name"`

	// File must exist. Optional: Type (file, dir, or symlink), Mode,
	// Owner (UID[:GID]), Contains (a regular expression the content must
	// match), and Target (a symlink's target).
	File     string `yaml:"file"`
	Type     string `yaml:"type"`
	Mode     string `yaml:"mode"`
	Owner    string `yaml:"owner"`
	Contains string `yaml:"contains"`
	Target   string `yaml:"target"`

	// Absent must not exist.
	Absent string `yaml:"absent"`

	// Config fields that are set must match the image config.
	Config *Config `yaml:"config"`

	mode     int64
	owner    *image.Owner
	contains *regexp.Regexp
}

// Config is the expected image config. Env, Labels, and Expose are
// subsets: other entries may exist.
type Config struct {
	User       *string           `yaml:"user"`
	WorkDir    *string           `yaml:"workdir"`
	Entrypoint *[]string         `yaml:"entrypoint"`
	Cmd        *[]string         `yaml:"cmd"`
	Env        map[string]string `yaml:"env"`
	Labels     map[string]string `yaml:"labels"`
	Expose     []string          `yaml:"expose"`
	StopSignal *string           `yaml:"stop-signal"`
}

func (c *Check) UnmarshalYAML(n *yaml.Node) error {
	type plain Check
	var p plain
	if err := n.Decode(&p); err != nil {
		return err
	}
	*c = Check(p)
	if err := c.Validate(); err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	return nil
}

// Validate checks that c is well formed and prepares it to run.
func (c *Check) Validate() error {
	var kinds []string
	if c.File != "" {
		kinds = append(kinds, "file")
	}
	if c.Absent != "" {
		kinds = append(kinds, "absent")
	}
	if c.Config != nil {
		kinds = append(kinds, "config")
	}
	if len(kinds) != 1 {
		return fmt.Errorf("test: want exactly one of file, absent, or config; got %v", kinds)
	}
	for _, p := range []string{c.File, c.Absent} {
		if p != "" && !path.IsAbs(p) {
			return fmt.Errorf("test %s: path must be absolute", c)
		}
	}
	if c.File == "" && (c.Type != "" || c.Mode != "" || c.Owner != "" || c.Contains != "" || c.Target != "") {
		return fmt.Errorf("test %s: type, mode, owner, contains, and target only apply to file", c)
	}
	if c.Type != "" && !slices.Contains([]string{"file", "dir", "symlink"}, c.Type) {
		return fmt.Errorf("test %s: type %q: want file, dir, or symlink", c, c.Type)
	}
	if c.Target != "" {
		if c.Type != "" && c.Type != "symlink" {
			return fmt.Errorf("test %s: target needs type symlink", c)
		}
		c.Type = "symlink"
	}
	if c.Contains != "" && c.Type != "" && c.Type != "file" {
		return fmt.Errorf("test %s: contains needs type file", c)
	}
	if c.Mode != "" {
		m, err := image.ParseMode(c.Mode)
		if err != nil {
			return fmt.Errorf("test %s: %w", c, err)
		}
		c.mode = m
	}
	if c.Owner != "" {
		o, err := image.ParseOwner(c.Owner)
		if err != nil {
			return fmt.Errorf("test %s: %w", c, err)
		}
		c.owner = &o
	}
	if c.Contains != "" {
		re, err := regexp.Compile(c.Contains)
		if err != nil {
			return fmt.Errorf("test %s: contains: %w", c, err)
		}
		c.contains = re
	}
	if c.Config != nil {
		for _, p := range c.Config.Expose {
			if _, err := image.ParsePort(p); err != nil {
				return fmt.Errorf("test %s: %w", c, err)
			}
		}
	}
	return nil
}

func (c *Check) String() string {
	switch {
	case c.Name != "":
		return c.Name
	case c.File != "":
		return "file " + c.File
	case c.Absent != "":
		return "absent " + c.Absent
	default:
		return "config"
	}
}

// Run checks img and returns one message per failed check. An error
// means the image could not be read.
func Run(img v1.Image, checks []Check) ([]string, error) {
	for i := range checks {
		if err := checks[i].Validate(); err != nil {
			return nil, err
		}
	}
	var failures []string
	fail := func(c *Check, format string, a ...any) {
		failures = append(failures, c.String()+": "+fmt.Sprintf(format, a...))
	}

	files, err := readFiles(img, checks)
	if err != nil {
		return nil, err
	}
	var cf *v1.ConfigFile
	for i := range checks {
		c := &checks[i]
		switch {
		case c.Absent != "":
			if f, ok := files[path.Clean(c.Absent)]; ok {
				fail(c, "exists (%s)", typeName(f.hdr.Typeflag))
			}
		case c.File != "":
			f, ok := files[path.Clean(c.File)]
			if !ok {
				fail(c, "does not exist")
				continue
			}
			checkFile(c, f, fail)
		case c.Config != nil:
			if cf == nil {
				if cf, err = img.ConfigFile(); err != nil {
					return nil, err
				}
			}
			checkConfig(c, cf.Config, fail)
		}
	}
	return failures, nil
}

type file struct {
	hdr  tar.Header
	body []byte
	big  bool
}

// readFiles scans the flattened filesystem once for the checked paths.
func readFiles(img v1.Image, checks []Check) (map[string]*file, error) {
	want := map[string]bool{} // path -> read content
	for _, c := range checks {
		if c.File != "" {
			p := path.Clean(c.File)
			want[p] = want[p] || c.contains != nil
		}
		if c.Absent != "" {
			p := path.Clean(c.Absent)
			want[p] = want[p]
		}
	}
	files := map[string]*file{}
	if len(want) == 0 {
		return files, nil
	}
	rc := mutate.Extract(img)
	defer rc.Close()
	tr := tar.NewReader(rc)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return files, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read image filesystem: %w", err)
		}
		p := path.Clean("/" + hdr.Name)
		readBody, ok := want[p]
		if !ok {
			continue
		}
		f := &file{hdr: *hdr}
		if readBody && isReg(hdr.Typeflag) {
			body, err := io.ReadAll(io.LimitReader(tr, maxContains+1))
			if err != nil {
				return nil, fmt.Errorf("read %s: %w", p, err)
			}
			f.big = len(body) > maxContains
			f.body = body
		}
		files[p] = f
	}
}

func checkFile(c *Check, f *file, fail func(*Check, string, ...any)) {
	got := typeName(f.hdr.Typeflag)
	if c.Type != "" && got != c.Type {
		fail(c, "is a %s, want %s", got, c.Type)
		return
	}
	if c.Mode != "" && f.hdr.Mode&0o7777 != c.mode {
		fail(c, "mode %04o, want %04o", f.hdr.Mode&0o7777, c.mode)
	}
	if c.owner != nil && (f.hdr.Uid != c.owner.UID || f.hdr.Gid != c.owner.GID) {
		fail(c, "owner %d:%d, want %d:%d", f.hdr.Uid, f.hdr.Gid, c.owner.UID, c.owner.GID)
	}
	if c.Target != "" && f.hdr.Linkname != c.Target {
		fail(c, "points to %q, want %q", f.hdr.Linkname, c.Target)
	}
	if c.contains != nil {
		switch {
		case !isReg(f.hdr.Typeflag):
			fail(c, "is a %s, so it has no content to match", got)
		case f.big:
			fail(c, "is over %d MiB, too big for contains", maxContains>>20)
		case !c.contains.Match(f.body):
			fail(c, "content does not match %q", c.Contains)
		}
	}
}

func checkConfig(c *Check, got v1.Config, fail func(*Check, string, ...any)) {
	want := c.Config
	if want.User != nil && got.User != *want.User {
		fail(c, "user %q, want %q", got.User, *want.User)
	}
	if want.WorkDir != nil && got.WorkingDir != *want.WorkDir {
		fail(c, "workdir %q, want %q", got.WorkingDir, *want.WorkDir)
	}
	if want.StopSignal != nil && got.StopSignal != *want.StopSignal {
		fail(c, "stop-signal %q, want %q", got.StopSignal, *want.StopSignal)
	}
	if want.Entrypoint != nil && !slices.Equal(got.Entrypoint, *want.Entrypoint) {
		fail(c, "entrypoint %q, want %q", got.Entrypoint, *want.Entrypoint)
	}
	if want.Cmd != nil && !slices.Equal(got.Cmd, *want.Cmd) {
		fail(c, "cmd %q, want %q", got.Cmd, *want.Cmd)
	}
	env := map[string]string{}
	for _, kv := range got.Env {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	for _, k := range sortedKeys(want.Env) {
		if v, ok := env[k]; !ok {
			fail(c, "env %s is not set, want %q", k, want.Env[k])
		} else if v != want.Env[k] {
			fail(c, "env %s=%q, want %q", k, v, want.Env[k])
		}
	}
	for _, k := range sortedKeys(want.Labels) {
		if v, ok := got.Labels[k]; !ok {
			fail(c, "label %s is not set, want %q", k, want.Labels[k])
		} else if v != want.Labels[k] {
			fail(c, "label %s=%q, want %q", k, v, want.Labels[k])
		}
	}
	for _, p := range want.Expose {
		norm, _ := image.ParsePort(p)
		if _, ok := got.ExposedPorts[norm]; !ok {
			fail(c, "port %s is not exposed", norm)
		}
	}
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func isReg(t byte) bool { return t == tar.TypeReg || t == tar.TypeRegA }

func typeName(t byte) string {
	switch {
	case isReg(t):
		return "file"
	case t == tar.TypeDir:
		return "dir"
	case t == tar.TypeSymlink:
		return "symlink"
	case t == tar.TypeLink:
		return "hard link"
	default:
		return fmt.Sprintf("type %q entry", t)
	}
}
