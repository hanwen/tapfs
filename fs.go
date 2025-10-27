// Copyright 2020 the Go-FUSE Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package tapfs

import (
	"context"
	"fmt"
	"os"
	"sync"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

var (
	OpRead   = Operation(0)
	OpCreate = Operation(1)
	OpUpdate = Operation(2)
	OpDelete = Operation(3)
)

const opCount = 4

type Operation int

type openData struct {
	id string
	mu sync.Mutex

	deletions map[string]struct{}

	// TODO: what to do here? The inode may be evicted if we're under memory pressure, if so, we can't get at the path anymore.
	ops map[*fs.Inode]Operation
}

func (r *tapFSData) registerPGID(pgid int) *openData {
	r.mu.Lock()
	defer r.mu.Unlock()

	od := r.openDataByPGID[pgid]
	if od == nil {
		ns := r.lastID + 1
		r.lastID = ns
		od = &openData{
			id:        fmt.Sprintf("%d", ns),
			deletions: map[string]struct{}{},
			ops:       map[*fs.Inode]Operation{},
		}

		r.openDataByPGID[pgid] = od
	}
	return od
}

func (r *tapFSData) removeRecord(pgid int) *openData {
	r.mu.Lock()
	defer r.mu.Unlock()
	od := r.openDataByPGID[pgid]
	delete(r.openDataByPGID, pgid)
	return od
}

func parentPID(pid int) int {
	f, err := os.Open(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 1
	}
	defer f.Close()

	var ppid int
	var stat, name string
	n, err := fmt.Fscanf(f, "%d (%s) %c %d", &pid, &name, &stat, &ppid)
	if err != nil || n != 4 {
		return 1
	}
	return ppid
}

func (r *tapFSData) openData(pgid int) *openData {
	r.mu.Lock()
	defer r.mu.Unlock()

	for {
		od := r.openDataByPGID[pgid]
		if od != nil {
			return od
		}

		pgid = parentPID(pgid)
	}
}

func (od *openData) record(node *fs.Inode, path string, op Operation) {
	od.mu.Lock()
	defer od.mu.Unlock()

	if op == OpDelete {
		if node != nil && od.ops[node] == OpCreate {
			delete(od.ops, node)
			return
		}
		od.deletions[path] = struct{}{}
		return
	}

	before := od.ops[node]
	if before == OpCreate {
		return
	}
	if op == OpRead && before == OpUpdate {
		return
	}
	od.ops[node] = op
}

type tapFSNode struct {
	tapFSData *tapFSData

	mu sync.Mutex

	// TODO: Setattr should toggle FileType if applicable
	fileInfo FileInfo
}

type tapFSData struct {
	mu             sync.Mutex
	lastID         int64
	openDataByPGID map[int]*openData

	loopbackHashCache *localDiskDigestCache
}

func newTapFSData(cas CAS) *tapFSData {
	return &tapFSData{
		loopbackHashCache: newLoopbackHashCache(cas),
		openDataByPGID:    map[int]*openData{},
	}
}

func toHex(b []byte) string {
	return fmt.Sprintf("%x", b)
}

func context2pgid(ctx context.Context) int {
	fc := ctx.(*fuse.Context)
	pgid, err := syscall.Getpgid(int(fc.Caller.Pid))
	if err != nil {
		panic(err)
	}
	return pgid
}

func (od *openData) recordRename(src, dest *fs.Inode) {
	if dest != nil {
		od.record(src, "", OpUpdate)
	} else {
		// if src was not created, this should be something else
		od.record(src, "", OpCreate)
	}
}
