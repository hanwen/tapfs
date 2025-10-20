package tapfs

import (
	"fmt"
	"strings"
)

type ActionCacheValue struct {
	Command string

	Inputs  map[string]string
	Outputs map[string]string
}

func actionCacheKey(req *TraceRequest, hashes map[string]string) string {
	key := req.Command + strings.Join(req.DeclaredInputs, "\000") + strings.Join(req.DeclaredOutputs, "\000")
	for _, k := range req.DeclaredInputs {
		key += hashes[k] + "\000"
	}
	return key
}

func actionCacheValue(req *TraceRequest, rep *TraceResponse) (*ActionCacheValue, error) {
	if len(rep.Update) != 0 || len(rep.Delete) != 0 {
		return nil, fmt.Errorf("cannot cache updates or deletions")
	}

	e := &ActionCacheValue{
		Command: req.Command,
		Inputs:  map[string]string{},
		Outputs: map[string]string{},
	}
	for _, k := range rep.Create {
		e.Outputs[k] = rep.Hashes[k]
	}
	for _, k := range rep.Read {
		e.Inputs[k] = rep.Hashes[k]
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
