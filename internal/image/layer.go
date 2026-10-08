package image

import (
	"archive/tar"
	"bufio"
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

// LayerOptions controls how a layer is packed.
type LayerOptions struct {
	// Created stamps every entry's modification time.
	Created time.Time
	// MediaType is types.OCILayer or types.DockerLayer.
	MediaType types.MediaType
}

type entry struct {
	name     string // image path without the leading slash
	typ      byte
	mode     int64
	uid, gid int
	src      string // host file, for regular files read from disk
	data     []byte // contents, for inline regular files
	inline   bool
	link     string // symlink target
	size     int64
}

// Layer packs items into one deterministic layer. Entries are sorted and
// stamped with o.Created, so identical inputs give identical digests.
// Later items replace earlier ones at the same path.
//
// The file list is fixed when Layer is called, but host file contents are
// streamed from disk each time the layer is read (to hash, compress, and
// upload it), so a layer never has to fit in memory. Sources must not change
// until the image has been written.
func Layer(items []Item, o LayerOptions) (v1.Layer, error) {
	entries, err := collectEntries(items)
	if err != nil {
		return nil, err
	}
	opener := func() (io.ReadCloser, error) {
		pr, pw := io.Pipe()
		go func() {
			// If the reader stops early, writes fail with io.ErrClosedPipe
			// and this goroutine exits.
			pw.CloseWithError(writeTar(pw, entries, o.Created))
		}()
		return pr, nil
	}
	mt := o.MediaType
	if mt == "" {
		mt = types.OCILayer
	}
	return tarball.LayerFromOpener(opener, tarball.WithMediaType(mt))
}

// collectEntries resolves items into the sorted list of layer entries.
func collectEntries(items []Item) ([]entry, error) {
	byName := map[string]entry{}
	for _, it := range items {
		if err := it.Validate(); err != nil {
			return nil, err
		}
		if err := collect(byName, it); err != nil {
			return nil, err
		}
	}
	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	sort.Strings(names)
	entries := make([]entry, len(names))
	for i, n := range names {
		entries[i] = byName[n]
	}
	return entries, nil
}

// writeTar writes the uncompressed tar stream for entries to w.
func writeTar(w io.Writer, entries []entry, mtime time.Time) error {
	bw := bufio.NewWriterSize(w, 64<<10)
	tw := tar.NewWriter(bw)
	for _, e := range entries {
		hdr := &tar.Header{
			Typeflag: e.typ,
			Name:     e.name,
			Mode:     e.mode,
			Uid:      e.uid,
			Gid:      e.gid,
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
			return err
		}
		if e.typ != tar.TypeReg {
			continue
		}
		var err error
		if e.inline {
			_, err = io.Copy(tw, bytes.NewReader(e.data))
		} else {
			err = copyFile(tw, e.src, e.size)
		}
		if err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return bw.Flush()
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

// collect records the entries for one item.
func collect(byName map[string]entry, it Item) error {
	dst := path.Clean(it.Dst)
	switch it.Kind {
	case Dir:
		return put(byName, entry{name: dst, typ: tar.TypeDir, mode: modeOr(it.Mode, 0o755)}, it.Owner)
	case Symlink:
		return put(byName, entry{name: dst, typ: tar.TypeSymlink, mode: 0o777, link: it.Target}, it.Owner)
	case File:
		return put(byName, entry{
			name: dst, typ: tar.TypeReg, mode: modeOr(it.Mode, 0o644),
			data: it.Content, inline: true, size: int64(len(it.Content)),
		}, it.Owner)
	}

	info, err := os.Lstat(it.Src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		if strings.HasSuffix(it.Dst, "/") {
			dst = path.Join(dst, filepath.Base(it.Src))
		}
		e, err := hostEntry(dst, it.Src, info, it)
		if err != nil {
			return err
		}
		return put(byName, e, it.Owner)
	}
	return filepath.WalkDir(it.Src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(it.Src, p)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		e, err := hostEntry(path.Join(dst, filepath.ToSlash(rel)), p, info, it)
		if err != nil {
			return err
		}
		return put(byName, e, it.Owner)
	})
}

// hostEntry describes the host file src, stored at dst.
func hostEntry(dst, src string, info fs.FileInfo, it Item) (entry, error) {
	e := entry{name: dst, mode: tarMode(info.Mode())}
	switch {
	case info.Mode().IsRegular():
		e.typ = tar.TypeReg
		e.src = src
		e.size = info.Size()
		e.mode = modeOr(it.Mode, e.mode)
	case info.IsDir():
		e.typ = tar.TypeDir
		e.mode = modeOr(it.DirMode, e.mode)
	case info.Mode()&fs.ModeSymlink != 0:
		target, err := os.Readlink(src)
		if err != nil {
			return entry{}, err
		}
		e.typ = tar.TypeSymlink
		e.link = target
		// Link permissions differ between Linux and macOS and are ignored
		// by runtimes, so fix them for reproducible digests.
		e.mode = 0o777
	default:
		return entry{}, fmt.Errorf("%s: unsupported file type %s", src, info.Mode().Type())
	}
	return e, nil
}

func put(byName map[string]entry, e entry, owner *Owner) error {
	e.name = strings.TrimPrefix(e.name, "/")
	if e.name == "" {
		// The image root itself; it always exists.
		return nil
	}
	if owner != nil {
		e.uid, e.gid = owner.UID, owner.GID
	}
	byName[e.name] = e
	return nil
}

// tarMode converts a Go file mode to tar's chmod-style permission bits.
func tarMode(m fs.FileMode) int64 {
	mode := int64(m.Perm())
	if m&fs.ModeSetuid != 0 {
		mode |= 0o4000
	}
	if m&fs.ModeSetgid != 0 {
		mode |= 0o2000
	}
	if m&fs.ModeSticky != 0 {
		mode |= 0o1000
	}
	return mode
}

func modeOr(m *int64, def int64) int64 {
	if m != nil {
		return *m
	}
	return def
}
