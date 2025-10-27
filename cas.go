package tapfs

import (
	"bytes"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sync"
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
	newhash func() hash.Hash
	mu      sync.Mutex
	cache   map[Digest][]byte
}

func NewMemCAS(n func() hash.Hash) *memCAS {
	return &memCAS{
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
	io.Writer
	buf          *bytes.Buffer
	hash         hash.Hash
	cas          *memCAS
	expectedSize int64
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
	m := io.MultiWriter(h, buf)

	return &memCASWriter{
		Writer:       m,
		buf:          buf,
		hash:         h,
		cas:          c,
		expectedSize: expectedSize,
	}, nil
}

var _ = (CAS)((*diskCAS)(nil))

type diskCAS struct {
	dir string

	newhash  func() hash.Hash
	zeroHash string
}

func NewDiskCAS(dir string, newhash func() hash.Hash) *diskCAS {
	z := fmt.Sprintf("%x", make([]byte, len(newhash().Sum(nil))))
	return &diskCAS{
		dir:      dir,
		newhash:  newhash,
		zeroHash: z,
	}
}

func (c *diskCAS) path(d Digest) string {
	return filepath.Join(c.dir, d.String())
}

func (c *diskCAS) Zero() Digest { return Digest{c.zeroHash, 0} }

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
	cw.Writer = io.MultiWriter(cw.hash, f)
	return cw, nil
}
