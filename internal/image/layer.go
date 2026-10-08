// Package image builds OCI images from a base image plus local files and
// writes them to a registry, an OCI layout directory, or a tarball.
package image

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// Add copies Src on the host to Dst inside the image.
//
// A directory Src is copied recursively so that its contents land under Dst.
// A file Src is written to Dst, or to Dst/<basename> when Dst ends in "/".
type Add struct {
	Src string
	Dst string
}

// ParseAdd parses a SRC:DST flag value. DST must be an absolute path.
func ParseAdd(s string) (Add, error) {
	src, dst, ok := strings.Cut(s, ":")
	if !ok || src == "" || dst == "" {
		return Add{}, fmt.Errorf("add %q: want SRC:DST", s)
	}
	if !path.IsAbs(dst) {
		return Add{}, fmt.Errorf("add %q: destination must be an absolute path", s)
	}
	return Add{Src: src, Dst: dst}, nil
}

type entry struct {
	name string // image path without the leading slash
	typ  byte
	mode int64
	src  string // host file for regular files
	link string // symlink target
	size int64
}

// Layer packs adds into a single deterministic layer. Entries are sorted,
// owned by root, and stamped with mtime, so identical inputs give identical
// digests. mediaType must be types.OCILayer or types.DockerLayer.
func Layer(adds []Add, mtime time.Time, mediaType types.MediaType) (v1.Layer, error) {
	data, err := layerTar(adds, mtime)
	if err != nil {
		return nil, err
	}
	opener := func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(data)), nil
	}
	return tarball.LayerFromOpener(opener, tarball.WithMediaType(mediaType))
}

// layerTar returns the uncompressed tar stream for adds.
func layerTar(adds []Add, mtime time.Time) ([]byte, error) {
	entries := map[string]entry{}
	for _, a := range adds {
		if err := collect(entries, a); err != nil {
			return nil, err
		}
	}

	names := make([]string, 0, len(entries))
	for n := range entries {
		names = append(names, n)
	}
	sort.Strings(names)

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, n := range names {
		e := entries[n]
		hdr := &tar.Header{
			Typeflag: e.typ,
			Name:     e.name,
			Mode:     e.mode,
			ModTime:  mtime,
			Format:   tar.FormatPAX,
		}
		switch e.typ {
		case tar.TypeDir:
			hdr.Name += "/"
		case tar.TypeSymlink:
			hdr.Linkname = e.link
		case tar.TypeReg:
			hdr.Size = e.size
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, err
		}
		if e.typ == tar.TypeReg {
			if err := copyFile(tw, e.src, e.size); err != nil {
				return nil, err
			}
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func copyFile(w io.Writer, name string, size int64) error {
	f, err := os.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	n, err := io.Copy(w, io.LimitReader(f, size))
	if err != nil {
		return fmt.Errorf("read %s: %w", name, err)
	}
	if n != size {
		return fmt.Errorf("read %s: file changed size during build", name)
	}
	return nil
}

// collect records the entries for one Add. Later adds replace earlier ones
// at the same path.
func collect(entries map[string]entry, a Add) error {
	info, err := os.Lstat(a.Src)
	if err != nil {
		return err
	}
	dst := path.Clean(a.Dst)
	if !info.IsDir() {
		if strings.HasSuffix(a.Dst, "/") {
			dst = path.Join(dst, filepath.Base(a.Src))
		}
		return put(entries, dst, a.Src, info)
	}

	return filepath.WalkDir(a.Src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(a.Src, p)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		return put(entries, path.Join(dst, filepath.ToSlash(rel)), p, info)
	})
}

func put(entries map[string]entry, dst, src string, info fs.FileInfo) error {
	name := strings.TrimPrefix(dst, "/")
	if name == "" {
		// The image root itself; it always exists.
		return nil
	}
	e := entry{name: name, mode: int64(info.Mode().Perm())}
	switch {
	case info.Mode().IsRegular():
		e.typ = tar.TypeReg
		e.src = src
		e.size = info.Size()
	case info.IsDir():
		e.typ = tar.TypeDir
	case info.Mode()&fs.ModeSymlink != 0:
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		e.typ = tar.TypeSymlink
		e.link = target
	default:
		return fmt.Errorf("%s: unsupported file type %s", src, info.Mode().Type())
	}
	entries[name] = e

	// Make sure every parent directory has its own entry, so runtimes that
	// unpack the layer do not have to invent permissions for them.
	for dir := path.Dir(name); dir != "."; dir = path.Dir(dir) {
		if _, ok := entries[dir]; ok {
			break
		}
		entries[dir] = entry{name: dir, typ: tar.TypeDir, mode: 0o755}
	}
	return nil
}
