// Copyright 2020 the Go-FUSE Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package tapfs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

var (
	opRead   = operation(0)
	opCreate = operation(1)
	opUpdate = operation(2)
	opDelete = operation(3)
)

const opCount = 4

type operation int

type openData struct {
	id  string
	mu  sync.Mutex
	ops map[string]operation
}

func (r *TapFSRoot) registerPGID(pgid int) *openData {
	r.mu.Lock()
	defer r.mu.Unlock()

	od := r.openDataByPGID[pgid]
	if od == nil {
		ns := r.lastID + 1
		r.lastID = ns
		od = &openData{
			id:  fmt.Sprintf("%d", ns),
			ops: map[string]operation{},
		}

		r.openDataByPGID[pgid] = od
	}
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

func (r *TapFSRoot) openData(pgid int) *openData {
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

func (od *openData) record(path string, op operation) {
	if path == "" {
		return
	}

	od.mu.Lock()
	defer od.mu.Unlock()

	update(op, od.ops, path)
}

func update(op operation, ops map[string]operation, path string) {
	before := ops[path]
	if before == opCreate {
		if op == opDelete {
			delete(ops, path)
		}
		return
	}
	if op == opRead && before == opUpdate {
		return
	}
	ops[path] = op
}

type TapFSRoot struct {
	TapFSNode

	mu             sync.Mutex
	lastID         int64
	openDataByPGID map[int]*openData
}

func (r *TapFSRoot) removeRecord(pgid int) *openData {
	r.mu.Lock()
	defer r.mu.Unlock()
	od := r.openDataByPGID[pgid]
	delete(r.openDataByPGID, pgid)
	return od
}

func (r *TapFSRoot) Access(ctx context.Context, mask uint32) syscall.Errno {
	return syscall.ENOSYS
}

func (r *TapFSRoot) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	if name == socketName {
		ch := r.GetChild(name)
		if ch != nil {
			return ch, 0
		}
	}
	return r.TapFSNode.Lookup(ctx, name, out)
}

type TapFSNode struct {
	*fs.LoopbackNode
}

func (n *TapFSNode) root() *TapFSRoot {
	return n.Root().Operations().(*TapFSRoot)
}

var _ = (fs.NodeWrapChilder)((*TapFSNode)(nil))

func (n *TapFSNode) WrapChild(ctx context.Context, ops fs.InodeEmbedder) fs.InodeEmbedder {
	if ln, ok := ops.(*fs.LoopbackNode); ok {
		return &TapFSNode{LoopbackNode: ln}
	}

	return ops
}

var _ = (fs.NodeOpener)((*TapFSNode)(nil))

func context2pgid(ctx context.Context) int {
	fc := ctx.(*fuse.Context)
	pgid, err := syscall.Getpgid(int(fc.Caller.Pid))
	if err != nil {
		panic(err)
	}
	return pgid
}

func (n *TapFSNode) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	fh, retFlags, errno := n.LoopbackNode.Open(ctx, flags)

	op := opRead
	if (flags & (syscall.O_APPEND | syscall.O_TRUNC | syscall.O_RDWR | syscall.O_WRONLY)) != 0 {
		op = opUpdate
	}
	n.root().openData(context2pgid(ctx)).record(n.Path(nil), op)
	return fh, retFlags, errno
}

func (n *TapFSNode) Getxattr(ctx context.Context, attr string, dest []byte) (uint32, syscall.Errno) {
	return 0, syscall.ENOSYS
}

var _ = (fs.NodeCreater)((*TapFSNode)(nil))

func (n *TapFSNode) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	inode, fh, flags, errno := n.LoopbackNode.Create(ctx, name, flags, mode, out)
	if errno == 0 {
		n.root().openData(context2pgid(ctx)).record(filepath.Join(n.Path(nil), name), opCreate)
	}

	return inode, fh, flags, errno
}

func (n *TapFSNode) Unlink(ctx context.Context, name string) syscall.Errno {
	errno := n.LoopbackNode.Unlink(ctx, name)
	if errno == 0 {
		n.root().openData(context2pgid(ctx)).record(filepath.Join(n.Path(nil), name), opDelete)
	}
	return errno
}

func (n *TapFSNode) Rename(ctx context.Context, name string, newParent fs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	errno := n.LoopbackNode.Rename(ctx, name, newParent, newName, flags)
	if errno == 0 {
		od := n.root().openData(context2pgid(ctx))
		od.record(filepath.Join(n.Path(nil), name), opDelete)
		od.record(filepath.Join(newParent.EmbeddedInode().Path(nil), newName), opCreate)
	}
	return errno
}

func NewTapFS(root *fs.LoopbackNode) *TapFSRoot {
	tapRoot := &TapFSRoot{
		TapFSNode:      TapFSNode{LoopbackNode: root},
		openDataByPGID: map[int]*openData{},
	}
	tapRoot.registerPGID(1)
	return tapRoot
}
