package tapfs

import (
	"fmt"
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

type ActionCache struct {
	// TODO serialize (sqlite?)
	cache map[string]*ActionCacheValue
}

func NewActionCache() *ActionCache {
	return &ActionCache{
		cache: map[string]*ActionCacheValue{},
	}
}

func (ac *ActionCache) Put(key string, val *ActionCacheValue) error {
	ac.cache[key] = val
	return nil
}

func (ac *ActionCache) Get(key string) (*ActionCacheValue, error) {
	return ac.cache[key], nil
}
