package tapfs

import (
	"bytes"
	"crypto/sha1"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/hanwen/grit/gritfs"
)

type Digest struct {
	Hash string
	Size uint64
}

func (d *Digest) String() string {
	return fmt.Sprintf("%x_%d", d.Hash, d.Size)
}

type CASWriter interface {
	io.WriteCloser
	Digest() Digest
}

type CAS interface {
	Get(Digest) (io.ReadCloser, error)
	NewWriter(size int64) (CASWriter, error)
}

type memCAS struct {
	git     bool
	newhash func() hash.Hash
	mu      sync.Mutex
	cache   map[Digest][]byte
}

func NewMemCAS(n func() hash.Hash, git bool) *memCAS {
	return &memCAS{
		git:     git,
		newhash: n,
		cache:   make(map[Digest][]byte),
	}
}

type bufCloser struct {
	*bytes.Buffer
}

func (c *bufCloser) Close() error { return nil }

func (c *memCAS) Get(d Digest) (io.ReadCloser, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, ok := c.cache[d]
	if !ok {
		return nil, nil
	}
	return &bufCloser{bytes.NewBuffer(b)}, nil
}

type memCASWriter struct {
	buf          *bytes.Buffer
	hash         hash.Hash
	cas          *memCAS
	expectedSize int64
}

func (w *memCASWriter) Write(b []byte) (int, error) {
	w.hash.Write(b)
	return w.buf.Write(b)
}

func (w *memCASWriter) Close() error {
	if w.expectedSize >= 0 && w.expectedSize != int64(w.buf.Len()) {
		return fmt.Errorf("got %d want %d", w.buf.Len(), w.expectedSize)
	}
	d := w.Digest()

	w.cas.mu.Lock()
	defer w.cas.mu.Unlock()
	w.cas.cache[d] = w.buf.Bytes()
	return nil
}

func (w *memCASWriter) Digest() Digest {
	return Digest{
		Hash: toHex(w.hash.Sum(nil)),
		Size: uint64(w.buf.Len()),
	}
}

func (c *memCAS) NewWriter(expectedSize int64) (CASWriter, error) {
	h := c.newhash()
	buf := &bytes.Buffer{}

	if c.git {
		fmt.Fprintf(h, "blob %d\000", expectedSize)
	}
	return &memCASWriter{
		buf:          buf,
		hash:         h,
		cas:          c,
		expectedSize: expectedSize,
	}, nil
}

var _ = (CAS)((*diskCAS)(nil))

type diskCAS struct {
	dir      string
	git      bool
	newhash  func() hash.Hash
	zeroHash string
}

func NewDiskCAS(dir string, newhash func() hash.Hash, git bool) *diskCAS {
	return &diskCAS{
		dir:     dir,
		newhash: newhash,
		git:     git,
	}
}

func (c *diskCAS) path(d Digest) string {
	return filepath.Join(c.dir, d.String())
}

func (c *diskCAS) Get(d Digest) (io.ReadCloser, error) {
	f, err := os.Open(c.path(d))
	return f, err
}

type diskCasWriter struct {
	io.Writer
	dest         *os.File
	hash         hash.Hash
	size         int64
	expectedSize int64
	cas          *diskCAS
}

func (w *diskCasWriter) Write(b []byte) (int, error) {
	n, err := w.Writer.Write(b)
	w.size += int64(n)
	return n, err
}

func (w *diskCasWriter) Close() error {
	err := w.dest.Close()
	if err != nil {
		return err
	}
	if w.expectedSize >= 0 && w.expectedSize != w.size {
		return fmt.Errorf("got size %d want %d", w.size, w.expectedSize)
	}

	digest := w.Digest()
	if err := os.Rename(w.dest.Name(), w.cas.path(digest)); err != nil {
		os.Remove(w.dest.Name())
		return err
	}
	return nil
}

func (w *diskCasWriter) Digest() Digest {
	return Digest{
		Hash: toHex(w.hash.Sum(nil)),
		Size: uint64(w.size),
	}
}

func (c *diskCAS) NewWriter(size int64) (CASWriter, error) {
	f, err := os.CreateTemp(c.dir, "")
	if err != nil {
		return nil, err
	}

	cw := &diskCasWriter{expectedSize: size,
		dest: f, cas: c, hash: c.newhash()}
	if c.git {
		fmt.Fprintf(cw.hash, "blob %d\000", size)
	}
	cw.Writer = io.MultiWriter(cw.hash, f)
	return cw, nil
}

type gritCASAdapter struct {
	cas     *gritfs.CAS
	newHash func() hash.Hash
}

func NewGritCASAdapter(gc *gritfs.CAS) *gritCASAdapter {
	return &gritCASAdapter{
		cas:     gc,
		newHash: sha1.New,
	}
}

func (a *gritCASAdapter) Get(d Digest) (io.ReadCloser, error) {
	h := plumbing.NewHash(d.Hash)

	f, ok := a.cas.Open(h)
	if !ok {
		return nil, os.ErrNotExist
	}

	return f, nil
}

type gritCASWriter struct {
	*bytes.Buffer
	hash hash.Hash
	sz   int64
	cas  *gritCASAdapter
}

func (w *gritCASWriter) Write(b []byte) (int, error) {
	w.hash.Write(b)
	return w.Buffer.Write(b)
}

func (w *gritCASWriter) Close() error {
	if int64(w.Buffer.Len()) != w.sz {
		return fmt.Errorf("mismatch %d %d", w.Buffer.Len(), w.sz)
	}

	var p plumbing.Hash
	copy(p[:], w.hash.Sum(nil))
	return w.cas.cas.Write(p, w.Bytes())
}

func (w *gritCASWriter) Digest() Digest {
	return Digest{
		Hash: toHex(w.hash.Sum(nil)),
		Size: uint64(w.sz),
	}
}

func (a *gritCASAdapter) NewWriter(expectedSize int64) (CASWriter, error) {
	if expectedSize < 0 {
		return nil, fmt.Errorf("git hash needs size")
	}
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\000", expectedSize)
	return &gritCASWriter{
		hash:   h,
		sz:     expectedSize,
		Buffer: bytes.NewBuffer(make([]byte, expectedSize)),
		cas:    a,
	}, nil
}
