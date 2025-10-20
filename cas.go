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

func (c *CAS) Add(r io.Reader) (string, error) {
	f, err := os.CreateTemp(c.dir, "")
	if err != nil {
		return "", err
	}
	defer f.Close()
	hw := c.newhash()

	mw := io.MultiWriter(hw, f)
	if _, err := io.Copy(mw, r); err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}

	h := fmt.Sprintf("%x", hw.Sum(nil))
	if err := os.Rename(f.Name(), c.path(h)); err != nil {
		os.Remove(f.Name())
		return "", err
	}

	return string(h), nil
}
