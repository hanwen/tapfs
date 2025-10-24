package tapfs

import (
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
)

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

func (c *CAS) path(h string) string {
	return filepath.Join(c.dir, h)
}

func (c *CAS) Zero() string { return c.zeroHash }

func (c *CAS) Has(h string) bool {
	fi, _ := os.Stat(c.path(h))
	return fi != nil
}

func (c *CAS) Get(h string) (*os.File, error) {
	return os.Open(c.path(h))
}

type CasWriter struct {
	io.Writer
	dest *os.File
	hash hash.Hash
	cas  *CAS
}

func (w *CasWriter) Close() error {
	err := w.dest.Close()
	if err != nil {
		return err
	}

	h := w.Hash()
	if err := os.Rename(w.dest.Name(), w.cas.path(h)); err != nil {
		os.Remove(w.dest.Name())
		return err
	}
	return nil
}

func (w *CasWriter) Hash() string {
	return toHex(w.hash.Sum(nil))
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

func (c *CAS) Add(r io.Reader) (string, error) {
	w, err := c.NewWriter()
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(w, r); err != nil {
		return "", err
	}
	if err := w.Close(); err != nil {
		return "", err
	}

	return w.Hash(), nil
}
