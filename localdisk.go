package tapfs

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

type localDiskDigestCache struct {
	cas   CAS
	mu    sync.Mutex
	cache map[loopbackHashKey]Digest
}

func newLoopbackHashCache(cas CAS) *localDiskDigestCache {
	return &localDiskDigestCache{
		cas:   cas,
		cache: map[loopbackHashKey]Digest{},
	}
}

func (s *localDiskDigestCache) hashForPath(p string, st *syscall.Stat_t) (digest Digest, err error) {
	err = func() error {
		if err := syscall.Stat(p, st); err != nil {
			return err
		}

		var key loopbackHashKey
		var ok bool
		key.FromStat(st)
		s.mu.Lock()
		digest, ok = s.cache[key]
		s.mu.Unlock()

		if !ok {
			src, err := os.Open(p)
			if err != nil {
				return err
			}

			fi, err := src.Stat()
			if err != nil {
				return err
			}
			w, err := s.cas.NewWriter(fi.Size())
			if err != nil {
				return err
			}
			if _, err := io.Copy(w, src); err != nil {
				return err
			}
			if err := w.Close(); err != nil {
				return err
			}

			digest = w.Digest()
			s.mu.Lock()
			s.cache[key] = digest
			s.mu.Unlock()
		}
		return nil
	}()
	return digest, err
}

type loopbackHashKey struct {
	Ino  uint64
	Mtim syscall.Timespec
	Size int64
}

func (k *loopbackHashKey) FromStat(st *syscall.Stat_t) {
	k.Ino = st.Ino
	k.Mtim = st.Mtim
	k.Size = st.Size
}

func (s *localDiskDigestCache) copyTo(path string, digest Digest, typ FileType) error {
	r, err := s.cas.Get(digest)
	if err != nil {
		return err
	}
	if r == nil {
		return fmt.Errorf("digest not in cache: %#v", digest)
	}
	mode := 0644
	switch typ {
	case FileExecutable:
		mode = 0755
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, os.FileMode(mode))
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(f, r); err != nil {
		return err
	}

	var st syscall.Stat_t
	if err := syscall.Fstat(int(f.Fd()), &st); err != nil {
		return err
	}

	var key loopbackHashKey
	key.FromStat(&st)
	s.mu.Lock()
	s.cache[key] = digest
	s.mu.Unlock()
	if err := f.Close(); err != nil {
		return err
	}
	r.Close()
	return nil
}
