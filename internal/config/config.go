// Package config reads construct.yaml build files.
//
// A file describes one image, or several under images:. Top-level fields
// are defaults for every image in images:. Settings (base, user, ...)
// replace the defaults; lists of things (layers, env, expose, volumes)
// accumulate, and labels and annotations merge key by key.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/pkar/construct/internal/check"
	"github.com/pkar/construct/internal/image"
)

// Image is the build description of one image. Nil fields are unset.
type Image struct {
	Name string `yaml:"name"`

	Base      *string    `yaml:"base"`
	Platforms StringList `yaml:"platforms"`
	Tags      StringList `yaml:"tags"`

	Push      *bool   `yaml:"push"`
	OCILayout *string `yaml:"oci-layout"`
	Tarball   *string `yaml:"tarball"`
	Load      *bool   `yaml:"load"`
	Engine    *string `yaml:"engine"`
	Insecure  *bool   `yaml:"insecure"`
	VCS       *bool   `yaml:"vcs"`

	Compression      *string `yaml:"compression"`
	CompressionLevel *int    `yaml:"compression-level"`

	Rootfs *Rootfs `yaml:"rootfs"`
	Layers []Layer `yaml:"layers"`

	Entrypoint  *Command          `yaml:"entrypoint"`
	Cmd         *Command          `yaml:"cmd"`
	Env         EnvList           `yaml:"env"`
	Labels      map[string]string `yaml:"labels"`
	Annotations map[string]string `yaml:"annotations"`
	Expose      StringList        `yaml:"expose"`
	Volumes     StringList        `yaml:"volumes"`
	StopSignal  *string           `yaml:"stop-signal"`
	User        *string           `yaml:"user"`
	WorkDir     *string           `yaml:"workdir"`

	Tests []check.Check `yaml:"tests"`
}

// Rootfs asks construct to generate the files a minimal image needs; see
// image.Rootfs. ca-certs and tzdata are host paths or "system".
type Rootfs struct {
	Skeleton *bool    `yaml:"skeleton"`
	Users    []string `yaml:"users"`
	CACerts  *string  `yaml:"ca-certs"`
	Tzdata   *string  `yaml:"tzdata"`
}

// Image returns the rootfs as image.Rootfs; nil gives the zero value.
func (r *Rootfs) Image() image.Rootfs {
	if r == nil {
		return image.Rootfs{}
	}
	out := image.Rootfs{Users: r.Users}
	if r.Skeleton != nil {
		out.Skeleton = *r.Skeleton
	}
	if r.CACerts != nil {
		out.CACerts = *r.CACerts
	}
	if r.Tzdata != nil {
		out.Tzdata = *r.Tzdata
	}
	return out
}

// Layer is a named group of items.
type Layer struct {
	Name     string `yaml:"name"`
	Contents []Item `yaml:"contents"`
}

// Item is one layer entry. In YAML it is either the SRC:DST[:OPTIONS]
// string that -add takes, or a mapping with exactly one of src (with dst),
// mkdir, symlink (with target), or file (with content).
type Item struct{ image.Item }

func (it *Item) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		v, err := image.ParseAdd(n.Value)
		if err != nil {
			return lineErr(n, err)
		}
		it.Item = v
		return nil
	}
	var raw struct {
		Src     string  `yaml:"src"`
		Dst     string  `yaml:"dst"`
		Mkdir   string  `yaml:"mkdir"`
		Symlink string  `yaml:"symlink"`
		Target  string  `yaml:"target"`
		File    string  `yaml:"file"`
		Content *string `yaml:"content"`
		Mode    string  `yaml:"mode"`
		DirMode string  `yaml:"dirmode"`
		Owner   string  `yaml:"owner"`
	}
	if err := checkKeys(n, "src", "dst", "mkdir", "symlink", "target", "file", "content", "mode", "dirmode", "owner"); err != nil {
		return err
	}
	if err := n.Decode(&raw); err != nil {
		return err
	}
	var kinds []string
	var v image.Item
	if raw.Src != "" || raw.Dst != "" {
		kinds = append(kinds, "src")
		v = image.Item{Kind: image.Copy, Src: raw.Src, Dst: raw.Dst}
	}
	if raw.Mkdir != "" {
		kinds = append(kinds, "mkdir")
		v = image.Item{Kind: image.Dir, Dst: raw.Mkdir}
	}
	if raw.Symlink != "" {
		kinds = append(kinds, "symlink")
		v = image.Item{Kind: image.Symlink, Dst: raw.Symlink, Target: raw.Target}
	}
	if raw.File != "" {
		kinds = append(kinds, "file")
		v = image.Item{Kind: image.File, Dst: raw.File}
		if raw.Content != nil {
			v.Content = []byte(*raw.Content)
		}
	}
	if len(kinds) != 1 {
		return lineErr(n, fmt.Errorf("want exactly one of src, mkdir, symlink, or file; got %v", kinds))
	}
	if raw.Target != "" && v.Kind != image.Symlink {
		return lineErr(n, fmt.Errorf("target only applies to symlink"))
	}
	if raw.Content != nil && v.Kind != image.File {
		return lineErr(n, fmt.Errorf("content only applies to file"))
	}
	for k, val := range map[string]string{"mode": raw.Mode, "dirmode": raw.DirMode, "owner": raw.Owner} {
		if val == "" {
			continue
		}
		if err := v.SetOption(k, val); err != nil {
			return lineErr(n, err)
		}
	}
	if err := v.Validate(); err != nil {
		return lineErr(n, err)
	}
	it.Item = v
	return nil
}

// Command is an entrypoint or cmd: a list, or a string split on spaces.
// An empty list clears the base image's value.
type Command []string

func (c *Command) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		*c = strings.Fields(n.Value)
		return nil
	}
	var list []string
	if err := n.Decode(&list); err != nil {
		return err
	}
	*c = append(Command{}, list...)
	return nil
}

// StringList is a list, or a single string for a one-item list.
type StringList []string

func (s *StringList) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		*s = StringList{n.Value}
		return nil
	}
	var list []string
	if err := n.Decode(&list); err != nil {
		return err
	}
	*s = append(StringList{}, list...)
	return nil
}

// EnvList is a list of KEY=VALUE strings, or a mapping (applied in key
// order).
type EnvList []string

func (e *EnvList) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.MappingNode {
		var m map[string]string
		if err := n.Decode(&m); err != nil {
			return err
		}
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		out := make(EnvList, 0, len(keys))
		for _, k := range keys {
			out = append(out, k+"="+m[k])
		}
		*e = out
		return nil
	}
	var list StringList
	if err := n.Decode(&list); err != nil {
		return err
	}
	*e = EnvList(list)
	return nil
}

// File is a parsed construct.yaml.
type File struct {
	Defaults Image
	Images   []Image
	// Dir is the directory holding the file. Relative paths in the file
	// were resolved against it, and Git stamping reads it.
	Dir string
}

// Load reads and validates the build file at path.
func Load(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	f, err := Parse(data, filepath.Dir(abs))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return f, nil
}

// Parse parses a build file whose relative paths are relative to dir.
func Parse(data []byte, dir string) (*File, error) {
	var doc struct {
		Image  `yaml:",inline"`
		Images []Image `yaml:"images"`
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("empty build file")
		}
		return nil, err
	}
	f := &File{Defaults: doc.Image, Images: doc.Images, Dir: dir}
	if f.Defaults.Name != "" && len(f.Images) > 0 {
		return nil, fmt.Errorf("name: set it on each entry in images, not at the top level")
	}
	if len(f.Images) == 0 {
		f.Images = []Image{f.Defaults}
		f.Defaults = Image{}
	}
	seen := map[string]bool{}
	for i := range f.Images {
		im := &f.Images[i]
		if len(f.Images) > 1 && im.Name == "" {
			return nil, fmt.Errorf("images[%d]: every image needs a name when there are several", i)
		}
		if seen[im.Name] {
			return nil, fmt.Errorf("images: name %q used twice", im.Name)
		}
		seen[im.Name] = true
	}
	f.Defaults.resolve(dir)
	for i := range f.Images {
		f.Images[i].resolve(dir)
	}
	return f, nil
}

// Select returns the named images merged over the defaults, or every
// image when names is empty.
func (f *File) Select(names []string) ([]Image, error) {
	if len(names) == 0 {
		out := make([]Image, len(f.Images))
		for i, im := range f.Images {
			out[i] = Merge(f.Defaults, im)
		}
		return out, nil
	}
	var out []Image
	for _, n := range names {
		i := slices.IndexFunc(f.Images, func(im Image) bool { return im.Name == n })
		if i < 0 {
			var known []string
			for _, im := range f.Images {
				known = append(known, im.Name)
			}
			return nil, fmt.Errorf("no image named %q; the build file has %s", n, strings.Join(known, ", "))
		}
		out = append(out, Merge(f.Defaults, f.Images[i]))
	}
	return out, nil
}

// resolve makes relative host paths absolute against dir.
func (im *Image) resolve(dir string) {
	abs := func(p *string) {
		if p != nil && *p != "" && !filepath.IsAbs(*p) {
			*p = filepath.Join(dir, *p)
		}
	}
	abs(im.OCILayout)
	abs(im.Tarball)
	if im.Rootfs != nil {
		for _, p := range []*string{im.Rootfs.CACerts, im.Rootfs.Tzdata} {
			if p != nil && *p != image.System {
				abs(p)
			}
		}
	}
	for i := range im.Layers {
		for j := range im.Layers[i].Contents {
			it := &im.Layers[i].Contents[j]
			if it.Kind == image.Copy {
				abs(&it.Src)
			}
		}
	}
}

// Merge returns base with over applied: set settings in over replace
// those in base, lists of things are appended, and labels and annotations
// merge with over winning.
func Merge(base, over Image) Image {
	out := base
	if over.Name != "" {
		out.Name = over.Name
	}
	set(&out.Base, over.Base)
	set(&out.Push, over.Push)
	set(&out.OCILayout, over.OCILayout)
	set(&out.Tarball, over.Tarball)
	set(&out.Load, over.Load)
	set(&out.Engine, over.Engine)
	set(&out.Insecure, over.Insecure)
	set(&out.VCS, over.VCS)
	set(&out.Compression, over.Compression)
	set(&out.CompressionLevel, over.CompressionLevel)
	set(&out.Entrypoint, over.Entrypoint)
	set(&out.Cmd, over.Cmd)
	set(&out.StopSignal, over.StopSignal)
	set(&out.User, over.User)
	set(&out.WorkDir, over.WorkDir)
	if over.Platforms != nil {
		out.Platforms = over.Platforms
	}
	if over.Tags != nil {
		out.Tags = over.Tags
	}
	out.Rootfs = mergeRootfs(base.Rootfs, over.Rootfs)
	out.Layers = concat(base.Layers, over.Layers)
	out.Env = concat(base.Env, over.Env)
	out.Expose = concat(base.Expose, over.Expose)
	out.Volumes = concat(base.Volumes, over.Volumes)
	out.Tests = concat(base.Tests, over.Tests)
	out.Labels = mergeMap(base.Labels, over.Labels)
	out.Annotations = mergeMap(base.Annotations, over.Annotations)
	return out
}

func mergeRootfs(base, over *Rootfs) *Rootfs {
	if base == nil {
		return over
	}
	if over == nil {
		return base
	}
	out := *base
	set(&out.Skeleton, over.Skeleton)
	set(&out.CACerts, over.CACerts)
	set(&out.Tzdata, over.Tzdata)
	out.Users = concat(base.Users, over.Users)
	return &out
}

func set[T any](dst **T, v *T) {
	if v != nil {
		*dst = v
	}
}

func concat[S ~[]E, E any](a, b S) S {
	if b == nil {
		return a
	}
	if a == nil {
		return b
	}
	return append(slices.Clip(a), b...)
}

func mergeMap(a, b map[string]string) map[string]string {
	if len(b) == 0 {
		return a
	}
	out := make(map[string]string, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

func checkKeys(n *yaml.Node, allowed ...string) error {
	if n.Kind != yaml.MappingNode {
		return lineErr(n, fmt.Errorf("want a mapping"))
	}
	for i := 0; i < len(n.Content); i += 2 {
		k := n.Content[i]
		if !slices.Contains(allowed, k.Value) {
			return lineErr(k, fmt.Errorf("unknown field %q; want one of %s", k.Value, strings.Join(allowed, ", ")))
		}
	}
	return nil
}

func lineErr(n *yaml.Node, err error) error {
	return fmt.Errorf("line %d: %w", n.Line, err)
}
