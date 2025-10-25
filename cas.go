package tapfs

import (
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
)

type Digest struct {
	Hash string
	Size uint64
}

func (d *Digest) String() string {
	return fmt.Sprintf("%x_%d", d.Hash, d.Size)
}

type CAS struct {
	dir string

	newhash  func() hash.Hash
	zeroHash string
}

func NewCAS(dir string, newhash func() hash.Hash) *CAS {
	z := fmt.Sprintf("%x", make([]byte, len(newhash().Sum(nil))))
	return &CAS{
		dir:      dir,
		newhash:  newhash,
		zeroHash: z,
	}
}

func (c *CAS) path(d Digest) string {
	return filepath.Join(c.dir, d.String())
}

func (c *CAS) Zero() Digest { return Digest{c.zeroHash, 0} }

func (c *CAS) Has(d Digest) bool {
	fi, _ := os.Stat(c.path(d))
	return fi != nil
}

func (c *CAS) Get(d Digest) (*os.File, error) {
	return os.Open(c.path(d))
}

type CasWriter struct {
	io.Writer
	dest *os.File
	hash hash.Hash
	size uint64
	cas  *CAS
}

func (w *CasWriter) Write(b []byte) (int, error) {
	n, err := w.Writer.Write(b)
	w.size += uint64(n)
	return n, err
}

func (w *CasWriter) Close() error {
	err := w.dest.Close()
	if err != nil {
		return err
	}

	digest := w.Digest()
	if err := os.Rename(w.dest.Name(), w.cas.path(digest)); err != nil {
		os.Remove(w.dest.Name())
		return err
	}
	return nil
}

func (w *CasWriter) Digest() Digest {
	return Digest{
		Hash: toHex(w.hash.Sum(nil)),
		Size: w.size,
	}
}

func (c *CAS) NewWriter() (*CasWriter, error) {
	f, err := os.CreateTemp(c.dir, "")
	if err != nil {
		return nil, err
	}

	cw := &CasWriter{dest: f, cas: c, hash: c.newhash()}
	cw.Writer = io.MultiWriter(cw.hash, f)
	return cw, nil
}

func (c *CAS) Add(r io.Reader) (Digest, error) {
	var dig Digest
	err := func() error {
		w, err := c.NewWriter()
		if err != nil {
			return err
		}
		if _, err := io.Copy(w, r); err != nil {
			return err
		}
		if err := w.Close(); err != nil {
			return err
		}
		dig = w.Digest()
		return nil
	}()
	return dig, err
}
