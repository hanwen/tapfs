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

func (r *TapFSRoot) registerPGID(pgid int) *openData {
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

func (od *openData) record(node *fs.Inode, path string, op Operation) {
	od.mu.Lock()
	defer od.mu.Unlock()

	if op == OpDelete {
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

	mu       sync.Mutex
	fileInfo FileInfo
}

func (n *TapFSNode) GetFileInfo(cas *CAS) (FileInfo, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	if n.fileInfo.Digest.Hash != "" {
		return n.fileInfo, nil
	}

	var fi FileInfo
	err := func() error {
		ctx := context.Background()
		var attr fuse.AttrOut
		errno := n.LoopbackNode.Getattr(ctx, nil, &attr)
		if errno != 0 {
			return errno
		}

		typ := FileRegular
		if attr.Mode&0111 != 0 {
			typ = FileExecutable
		}

		buf := make([]byte, 128<<10)
		fh, _, errno := n.LoopbackNode.Open(ctx, syscall.O_RDONLY)
		if errno != 0 {
			return errno
		}
		defer fh.(fs.FileReleaser).Release(ctx)

		w, err := cas.NewWriter()
		if err != nil {
			return err
		}
		var off int64
		for {
			res, errno := fh.(fs.FileReader).Read(ctx, buf, off)
			if errno != 0 {
				return errno
			}

			b, status := res.Bytes(buf)
			if status != 0 {
				return syscall.Errno(status)
			}

			w.Write(b)
			if len(b) < len(buf) {
				break
			}
			off += int64(len(b))
		}
		if err := w.Close(); err != nil {
			return err
		}
		fi = FileInfo{
			Digest: w.Digest(),
			Type:   typ,
		}
		return nil
	}()

	if err != nil {
		return FileInfo{}, err
	}

	n.fileInfo = fi
	return fi, nil
}

func toHex(b []byte) string {
	return fmt.Sprintf("%x", b)
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

	op := OpRead
	if (flags & (syscall.O_APPEND | syscall.O_TRUNC | syscall.O_RDWR | syscall.O_WRONLY)) != 0 {
		op = OpUpdate

		n.mu.Lock()
		n.fileInfo = FileInfo{}
		n.mu.Unlock()
	}
	n.root().openData(context2pgid(ctx)).record(n.EmbeddedInode(), "", op)
	return fh, retFlags, errno
}

func (n *TapFSNode) Getxattr(ctx context.Context, attr string, dest []byte) (uint32, syscall.Errno) {
	return 0, syscall.ENOSYS
}

var _ = (fs.NodeCreater)((*TapFSNode)(nil))

func (n *TapFSNode) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	inode, fh, flags, errno := n.LoopbackNode.Create(ctx, name, flags, mode, out)
	if errno == 0 {
		n.root().openData(context2pgid(ctx)).record(inode, "", OpCreate)
	}

	return inode, fh, flags, errno
}

func (n *TapFSNode) Unlink(ctx context.Context, name string) syscall.Errno {
	errno := n.LoopbackNode.Unlink(ctx, name)
	if errno == 0 {
		n.root().openData(context2pgid(ctx)).record(nil, filepath.Join(n.Path(nil), name), OpDelete)
	}
	return errno
}

func (n *TapFSNode) Rename(ctx context.Context, name string, newParent fs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	child := n.GetChild(name)
	errno := n.LoopbackNode.Rename(ctx, name, newParent, newName, flags)
	if errno == 0 {
		od := n.root().openData(context2pgid(ctx))

		od.record(nil, filepath.Join(n.Path(nil), name), OpDelete)
		od.record(child, "", OpCreate)
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
