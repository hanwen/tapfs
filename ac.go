package tapfs

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type ActionCacheValue struct {
	Command string

	Inputs  map[string]FileInfo
	Outputs map[string]FileInfo

	Stderr []byte
	Stdout []byte
}

func actionCacheKey(req *TraceRequest, hashes map[string]FileInfo) string {
	key := req.Command + strings.Join(req.DeclaredInputs, "\000") + strings.Join(req.DeclaredOutputs, "\000")
	for _, k := range req.DeclaredInputs {
		fi := hashes[k]
		key += fi.Digest.Hash + string([]byte{byte(fi.Type)})
	}
	return key
}

func actionCacheValue(req *TraceRequest, rep *TraceResponse) (*ActionCacheValue, error) {
	e := &ActionCacheValue{
		Command: req.Command,
		Inputs:  map[string]FileInfo{},
		Outputs: map[string]FileInfo{},
		Stdout:  req.Stdout,
		Stderr:  req.Stderr,
	}
	for p, op := range rep.Operations {
		if op == OpUpdate || op == OpDelete {
			return nil, fmt.Errorf("cannot cache updates or deletions: %s = %d", p, op)
		}
		if op == OpCreate {
			e.Outputs[p] = rep.Files[p]
		}
		if op == OpRead {
			e.Inputs[p] = rep.Files[p]
		}
	}
	return e, nil
}

type ActionCache interface {
	Put(key string, val *ActionCacheValue) error
	Get(key string) (*ActionCacheValue, error)
}

type memActionCache struct {
	// TODO serialize (sqlite?)
	cache map[string]*ActionCacheValue
}

func NewMemActionCache() *memActionCache {
	return &memActionCache{
		cache: map[string]*ActionCacheValue{},
	}
}

func (ac *memActionCache) Put(key string, val *ActionCacheValue) error {
	ac.cache[key] = val
	return nil
}

func (ac *memActionCache) Get(key string) (*ActionCacheValue, error) {
	return ac.cache[key], nil
}

type diskActionCache struct {
	dir string
}

func NewDiskActionCache(dir string) *diskActionCache {
	return &diskActionCache{dir}
}

func sha256hex(s string) Digest {
	h := sha256.New()
	io.WriteString(h, s)
	return Digest{fmt.Sprintf("%x", h.Sum(nil)), uint64(len(s))}
}

func (ac *diskActionCache) Put(key string, val *ActionCacheValue) error {
	k := sha256hex(key)
	valBytes, err := json.Marshal(val)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(ac.dir, k.Hash), valBytes, 0644)
}

func (ac *diskActionCache) Get(key string) (*ActionCacheValue, error) {
	k := sha256hex(key)
	c, err := os.ReadFile(filepath.Join(ac.dir, k.Hash))
	if os.IsNotExist(err) {
		return nil, nil
	}

	var val ActionCacheValue
	if err := json.Unmarshal(c, &val); err != nil {
		return nil, err
	}

	return &val, nil
}
