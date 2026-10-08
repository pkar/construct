package run

import (
	"archive/tar"
	"bufio"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// fileMeta is what Diff compares for one path of a filesystem export.
// Modification, access, and change times are ignored.
type fileMeta struct {
	typ      byte // tar.TypeReg for regular files and hard links
	hardlink bool
	mode     int64
	uid, gid int
	size     int64
	symlink  string
	devMajor int64
	devMinor int64
	xattrs   map[string]string // PAX records holding extended attributes
	hash     [sha256.Size]byte
	// origin is the entry whose contents a regular file has: its own
	// name, or the target of a hard link.
	origin string
}

func (m *fileMeta) same(o *fileMeta) bool {
	if m.typ != o.typ || m.mode != o.mode || m.uid != o.uid || m.gid != o.gid ||
		m.symlink != o.symlink || m.devMajor != o.devMajor || m.devMinor != o.devMinor ||
		len(m.xattrs) != len(o.xattrs) {
		return false
	}
	for k, v := range m.xattrs {
		if ov, ok := o.xattrs[k]; !ok || ov != v {
			return false
		}
	}
	return m.typ != tar.TypeReg || (m.size == o.size && m.hash == o.hash)
}

// ignoredTrees hold kernel and engine-managed files; changes below them
// never go into a layer.
var ignoredTrees = []string{"proc", "sys", "dev"}

func ignored(name string) bool {
	for _, t := range ignoredTrees {
		if strings.HasPrefix(name, t+"/") {
			return true
		}
	}
	return false
}

// cleanName turns a tar entry name into a path relative to the root,
// without a leading "./" or "/" or a trailing "/". The root is "".
func cleanName(n string) string {
	return strings.TrimPrefix(path.Clean("/"+n), "/")
}

// walkTar calls fn for every entry of the tar file, with names cleaned.
func walkTar(file string, fn func(name string, h *tar.Header, r io.Reader) error) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	tr := tar.NewReader(bufio.NewReaderSize(f, 256<<10))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read %s: %w", file, err)
		}
		if err := fn(cleanName(h.Name), h, tr); err != nil {
			return err
		}
	}
}

func xattrs(h *tar.Header) map[string]string {
	var m map[string]string
	for k, v := range h.PAXRecords {
		if strings.HasPrefix(k, "SCHILY.xattr.") {
			if m == nil {
				m = map[string]string{}
			}
			m[k] = v
		}
	}
	return m
}

// readIndex reads a filesystem export and hashes the contents of every
// regular file.
func readIndex(file string) (map[string]*fileMeta, error) {
	idx := map[string]*fileMeta{}
	err := walkTar(file, func(name string, h *tar.Header, r io.Reader) error {
		m := &fileMeta{
			typ:      h.Typeflag,
			mode:     h.Mode & 0o7777,
			uid:      h.Uid,
			gid:      h.Gid,
			devMajor: h.Devmajor,
			devMinor: h.Devminor,
			xattrs:   xattrs(h),
		}
		switch h.Typeflag {
		case tar.TypeReg, tar.TypeRegA:
			m.typ, m.size, m.origin = tar.TypeReg, h.Size, name
			hw := sha256.New()
			if _, err := io.Copy(hw, r); err != nil {
				return fmt.Errorf("read %s: %s: %w", file, name, err)
			}
			hw.Sum(m.hash[:0])
		case tar.TypeLink:
			target, ok := idx[cleanName(h.Linkname)]
			if !ok || target.typ != tar.TypeReg {
				return fmt.Errorf("read %s: hard link %s points to %s, which is not an earlier regular file", file, name, h.Linkname)
			}
			// A hard link shares the target's inode, so it has the same
			// contents and metadata.
			*m = *target
			m.hardlink = true
		case tar.TypeSymlink:
			m.symlink = h.Linkname
		case tar.TypeDir, tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
		case tar.TypeXGlobalHeader:
			return nil
		default:
			return fmt.Errorf("read %s: %s: unsupported tar entry type %q", file, name, h.Typeflag)
		}
		idx[name] = m
		return nil
	})
	return idx, err
}

// Stats counts what a diff layer holds.
type Stats struct {
	Changed int // added or changed entries, including parent directories
	Deleted int // whiteouts
}

// Diff compares two filesystem exports of the same container, before and
// after a change, and writes to out a layer tar that turns before into
// after: new and changed entries, their parent directories, and whiteouts
// for deleted paths. Entries are sorted, owned by their numeric IDs only,
// and stamped with mtime, so the same change always gives the same bytes.
// A hard link group keeps its links when all its new members are in the
// layer; the alphabetically first member holds the contents.
func Diff(before, after string, out io.Writer, mtime time.Time) (Stats, error) {
	var st Stats
	old, err := readIndex(before)
	if err != nil {
		return st, err
	}
	cur, err := readIndex(after)
	if err != nil {
		return st, err
	}

	emit := map[string]bool{}
	for n, m := range cur {
		if n == "" || ignored(n) {
			continue
		}
		if o, ok := old[n]; !ok || !o.same(m) {
			emit[n] = true
		}
	}
	for n := range emit {
		for p := path.Dir(n); p != "."; p = path.Dir(p) {
			if m, ok := cur[p]; ok && m.typ == tar.TypeDir {
				emit[p] = true
			}
		}
	}

	// A deleted path needs a whiteout unless a deleted or replaced
	// ancestor already hides it.
	whiteouts := map[string]bool{}
	for n := range old {
		if n == "" || ignored(n) {
			continue
		}
		if _, ok := cur[n]; ok || hiddenByAncestor(cur, n) {
			continue
		}
		whiteouts[whiteoutName(n)] = true
	}

	// Hard link groups: the first member in sort order holds the
	// contents and the others link to it.
	names := make([]string, 0, len(emit)+len(whiteouts))
	for n := range emit {
		names = append(names, n)
	}
	for n := range whiteouts {
		names = append(names, n)
	}
	sort.Strings(names)
	holder := map[string]string{} // origin -> member that holds the contents
	need := map[string]bool{}     // origins whose contents go into the layer
	for _, n := range names {
		m, ok := cur[n]
		if !emit[n] || !ok || m.typ != tar.TypeReg {
			continue
		}
		if _, ok := holder[m.origin]; !ok {
			holder[m.origin] = n
			need[m.origin] = true
		}
	}

	blobs, err := os.MkdirTemp(filepath.Dir(after), ".blobs-")
	if err != nil {
		return st, err
	}
	defer os.RemoveAll(blobs)
	blobFor := map[string]string{}
	err = walkTar(after, func(name string, h *tar.Header, r io.Reader) error {
		if !need[name] || (h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeRegA) {
			return nil
		}
		p := filepath.Join(blobs, fmt.Sprint(len(blobFor)))
		if err := saveBlob(p, r); err != nil {
			return err
		}
		blobFor[name] = p
		return nil
	})
	if err != nil {
		return st, err
	}

	bw := bufio.NewWriterSize(out, 256<<10)
	tw := tar.NewWriter(bw)
	for _, n := range names {
		if whiteouts[n] {
			st.Deleted++
			if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: n, ModTime: mtime, Format: tar.FormatPAX}); err != nil {
				return st, err
			}
			continue
		}
		st.Changed++
		m := cur[n]
		h := &tar.Header{
			Typeflag: m.typ,
			Name:     n,
			Mode:     m.mode,
			Uid:      m.uid,
			Gid:      m.gid,
			ModTime:  mtime,
			Devmajor: m.devMajor,
			Devminor: m.devMinor,
			Format:   tar.FormatPAX,
		}
		if len(m.xattrs) > 0 {
			h.PAXRecords = m.xattrs
		}
		var blob string
		switch m.typ {
		case tar.TypeDir:
			h.Name += "/"
		case tar.TypeSymlink:
			h.Linkname = m.symlink
		case tar.TypeReg:
			if hold := holder[m.origin]; hold != n {
				h.Typeflag, h.Linkname = tar.TypeLink, hold
				break
			}
			h.Size = m.size
			if blob = blobFor[m.origin]; blob == "" {
				return st, fmt.Errorf("read %s: contents of %s not found", after, m.origin)
			}
		}
		if err := tw.WriteHeader(h); err != nil {
			return st, err
		}
		if blob != "" {
			if err := copyBlob(tw, blob, m.size); err != nil {
				return st, err
			}
		}
	}
	if err := tw.Close(); err != nil {
		return st, err
	}
	return st, bw.Flush()
}

// hiddenByAncestor reports whether some ancestor of n is gone from cur or
// is no longer a directory there.
func hiddenByAncestor(cur map[string]*fileMeta, n string) bool {
	for p := path.Dir(n); p != "."; p = path.Dir(p) {
		if m, ok := cur[p]; !ok || m.typ != tar.TypeDir {
			return true
		}
	}
	return false
}

func whiteoutName(n string) string {
	dir, base := path.Split(n)
	return dir + ".wh." + base
}

func saveBlob(p string, r io.Reader) error {
	f, err := os.Create(p)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func copyBlob(w io.Writer, p string, size int64) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	n, err := io.Copy(w, f)
	if err != nil {
		return err
	}
	if n != size {
		return fmt.Errorf("%s: got %d bytes, want %d", p, n, size)
	}
	return nil
}
