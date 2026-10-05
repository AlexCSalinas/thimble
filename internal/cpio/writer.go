// Package cpio writes "newc" (SVR4) cpio archives, the format the Linux
// kernel unpacks as an initramfs. Only the subset needed to build a root
// filesystem is implemented: regular files, directories and symlinks.
//
// Everything is written as root:root regardless of who runs the builder,
// which is the whole reason this exists: macOS tar cannot produce root-owned
// entries without sudo, but cpio is just bytes.
package cpio

import (
	"fmt"
	"io"
	"path"
	"strings"
)

const (
	ModeDir     = 0o040000
	ModeRegular = 0o100000
	ModeSymlink = 0o120000
)

// Writer emits a newc archive. Parent directories are created implicitly so
// callers can add "lib/modules/x/kernel/foo.ko" without first adding each
// directory on the way down. The kernel's unpacker does not create parents.
type Writer struct {
	w     io.Writer
	ino   uint32
	seen  map[string]bool
	wrote int64
	err   error
}

func NewWriter(w io.Writer) *Writer {
	return &Writer{w: w, ino: 1, seen: map[string]bool{"": true, ".": true}}
}

func clean(name string) string {
	name = strings.TrimPrefix(name, "./")
	name = strings.TrimPrefix(name, "/")
	return path.Clean(name)
}

func (c *Writer) ensureParents(name string) error {
	dir := path.Dir(name)
	if c.seen[dir] {
		return nil
	}
	if err := c.ensureParents(dir); err != nil {
		return err
	}
	return c.Dir(dir, 0o755)
}

// Dir adds a directory. Adding one that already exists is a no-op.
func (c *Writer) Dir(name string, perm uint32) error {
	name = clean(name)
	if c.seen[name] {
		return nil
	}
	if err := c.ensureParents(name); err != nil {
		return err
	}
	c.seen[name] = true
	return c.entry(name, ModeDir|perm&0o7777, 0, nil)
}

// Symlink adds a symbolic link named name pointing at target.
func (c *Writer) Symlink(name, target string) error {
	name = clean(name)
	if err := c.ensureParents(name); err != nil {
		return err
	}
	c.seen[name] = true
	return c.entry(name, ModeSymlink|0o777, int64(len(target)), strings.NewReader(target))
}

// File adds a regular file of the given size whose contents come from r.
func (c *Writer) File(name string, perm uint32, size int64, r io.Reader) error {
	name = clean(name)
	if err := c.ensureParents(name); err != nil {
		return err
	}
	c.seen[name] = true
	return c.entry(name, ModeRegular|perm&0o7777, size, r)
}

// FileBytes is File for in-memory contents.
func (c *Writer) FileBytes(name string, perm uint32, b []byte) error {
	return c.File(name, perm, int64(len(b)), strings.NewReader(string(b)))
}

func (c *Writer) entry(name string, mode uint32, size int64, r io.Reader) error {
	if c.err != nil {
		return c.err
	}
	ino := c.ino
	c.ino++
	nlink := 1
	if mode&ModeDir != 0 {
		nlink = 2
	}
	hdr := fmt.Sprintf("070701%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x",
		ino, mode, 0, 0, nlink, 0, uint32(size), 0, 0, 0, 0, len(name)+1, 0)
	c.write([]byte(hdr))
	c.write([]byte(name))
	c.write([]byte{0})
	c.pad()
	if size > 0 {
		n, err := io.CopyN(c.w, r, size)
		c.wrote += n
		if err != nil && c.err == nil {
			c.err = fmt.Errorf("cpio: %s: %w", name, err)
		}
	}
	c.pad()
	return c.err
}

func (c *Writer) write(b []byte) {
	if c.err != nil {
		return
	}
	n, err := c.w.Write(b)
	c.wrote += int64(n)
	c.err = err
}

func (c *Writer) pad() {
	if rem := c.wrote % 4; rem != 0 {
		c.write(make([]byte, 4-rem))
	}
}

// Close writes the trailer. It does not close the underlying writer.
func (c *Writer) Close() error {
	if err := c.entry("TRAILER!!!", 0, 0, nil); err != nil {
		return err
	}
	// Kernel unpacker is happy with any length; 512-byte blocking matches
	// what GNU cpio emits and keeps tools like `cpio -t` quiet.
	if rem := c.wrote % 512; rem != 0 {
		c.write(make([]byte, 512-rem))
	}
	return c.err
}
