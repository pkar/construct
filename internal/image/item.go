// Package image builds OCI images from a base image plus local files and
// writes them to a registry, an OCI layout directory, a tarball, or a local
// container engine.
package image

import (
	"fmt"
	"path"
	"strconv"
	"strings"
)

// Kind says what an Item puts into a layer.
type Kind int

const (
	// Copy copies the host file or directory Src to Dst. A directory is
	// copied recursively so that its contents land under Dst. A file goes
	// to Dst, or to Dst/<basename> when Dst ends in "/".
	Copy Kind = iota
	// Dir creates an empty directory at Dst.
	Dir
	// Symlink creates a symbolic link at Dst that points to Target.
	Symlink
	// File writes Content to a regular file at Dst.
	File
)

// Owner is a numeric user and group ID.
type Owner struct{ UID, GID int }

// Item is one thing to put into a layer. Entries are owned by root and keep
// their host permission bits unless Owner, Mode, or DirMode say otherwise.
// Parent directories are not added implicitly, so a layer never resets the
// owner or mode of a directory that a lower layer created.
type Item struct {
	Kind    Kind
	Src     string // host path, for Copy
	Dst     string // absolute path in the image
	Target  string // link target, for Symlink
	Content []byte // file contents, for File

	// Mode sets the permission bits, in chmod's octal form including
	// setuid, setgid, and sticky, of regular files (Copy, File) and of the
	// directory made by Dir.
	Mode *int64
	// DirMode sets the permission bits of directories copied by Copy.
	DirMode *int64
	// Owner sets the owner of every entry the item creates.
	Owner *Owner
}

// LayerSpec is a named group of items that becomes one image layer.
// Splitting rarely-changing files (dependencies, assets) from often-changing
// ones (the application binary) into separate layers means a rebuild only
// uploads the layers whose contents changed.
type LayerSpec struct {
	Name  string
	Items []Item
}

// ParseAdd parses SRC:DST[:OPTIONS], where OPTIONS is a comma-separated
// list of mode=OCTAL, dirmode=OCTAL, and owner=UID[:GID].
func ParseAdd(s string) (Item, error) {
	src, rest, ok := strings.Cut(s, ":")
	dst, opts, _ := strings.Cut(rest, ":")
	if !ok || src == "" || dst == "" {
		return Item{}, fmt.Errorf("add %q: want SRC:DST[:OPTIONS]", s)
	}
	it := Item{Kind: Copy, Src: src, Dst: dst}
	if err := it.parseOptions(opts, "mode", "dirmode", "owner"); err != nil {
		return Item{}, fmt.Errorf("add %q: %w", s, err)
	}
	return it, it.Validate()
}

// ParseMkdir parses DST[:OPTIONS], where OPTIONS is a comma-separated list
// of mode=OCTAL and owner=UID[:GID].
func ParseMkdir(s string) (Item, error) {
	dst, opts, _ := strings.Cut(s, ":")
	it := Item{Kind: Dir, Dst: dst}
	if err := it.parseOptions(opts, "mode", "owner"); err != nil {
		return Item{}, fmt.Errorf("mkdir %q: %w", s, err)
	}
	return it, it.Validate()
}

// ParseSymlink parses DST:TARGET.
func ParseSymlink(s string) (Item, error) {
	dst, target, ok := strings.Cut(s, ":")
	if !ok {
		return Item{}, fmt.Errorf("symlink %q: want DST:TARGET", s)
	}
	it := Item{Kind: Symlink, Dst: dst, Target: target}
	return it, it.Validate()
}

func (it *Item) parseOptions(s string, allowed ...string) error {
	if s == "" {
		return nil
	}
	for _, kv := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || !contains(allowed, k) {
			return fmt.Errorf("option %q: want one of %s as KEY=VALUE", kv, strings.Join(allowed, ", "))
		}
		if err := it.SetOption(k, v); err != nil {
			return err
		}
	}
	return nil
}

// SetOption sets mode, dirmode, or owner from its text form.
func (it *Item) SetOption(key, value string) error {
	switch key {
	case "mode", "dirmode":
		m, err := ParseMode(value)
		if err != nil {
			return err
		}
		if key == "mode" {
			it.Mode = &m
		} else {
			it.DirMode = &m
		}
	case "owner":
		o, err := ParseOwner(value)
		if err != nil {
			return err
		}
		it.Owner = &o
	default:
		return fmt.Errorf("unknown option %q", key)
	}
	return nil
}

// ParseMode parses an octal permission mode such as 0755 or 4755.
func ParseMode(s string) (int64, error) {
	m, err := strconv.ParseInt(s, 8, 64)
	if err != nil || m < 0 || m > 0o7777 {
		return 0, fmt.Errorf("mode %q: want octal 0000-7777", s)
	}
	return m, nil
}

// ParseOwner parses UID[:GID]; the group defaults to the user ID.
func ParseOwner(s string) (Owner, error) {
	u, g, hasGroup := strings.Cut(s, ":")
	uid, err := strconv.Atoi(u)
	if err != nil || uid < 0 {
		return Owner{}, fmt.Errorf("owner %q: want numeric UID[:GID]", s)
	}
	gid := uid
	if hasGroup {
		if gid, err = strconv.Atoi(g); err != nil || gid < 0 {
			return Owner{}, fmt.Errorf("owner %q: want numeric UID[:GID]", s)
		}
	}
	return Owner{UID: uid, GID: gid}, nil
}

// Validate reports items that cannot be packed.
func (it Item) Validate() error {
	if !path.IsAbs(it.Dst) {
		return fmt.Errorf("%q: destination must be an absolute path", it.Dst)
	}
	switch it.Kind {
	case Copy:
		if it.Src == "" {
			return fmt.Errorf("%s: no source to copy", it.Dst)
		}
	case Dir:
		if it.DirMode != nil {
			return fmt.Errorf("%s: dirmode only applies to copied trees; use mode", it.Dst)
		}
	case Symlink:
		if it.Target == "" {
			return fmt.Errorf("%s: symlink needs a target", it.Dst)
		}
		if it.Mode != nil || it.DirMode != nil {
			return fmt.Errorf("%s: symlinks have no mode", it.Dst)
		}
		fallthrough
	case File:
		if path.Clean(it.Dst) == "/" || strings.HasSuffix(it.Dst, "/") {
			return fmt.Errorf("%q: want a file path, not a directory", it.Dst)
		}
	default:
		return fmt.Errorf("%s: unknown item kind %d", it.Dst, it.Kind)
	}
	return nil
}

// describe is the item's short form in image history. It names image paths
// only, never host paths, so history does not depend on the build machine.
func (it Item) describe() string {
	switch it.Kind {
	case Dir:
		return "mkdir " + it.Dst
	case Symlink:
		return "symlink " + it.Dst + " -> " + it.Target
	case File:
		return "file " + it.Dst
	default:
		return "add " + it.Dst
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
