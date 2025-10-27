package tapfs

import (
	"context"
	"path/filepath"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

type loopbackTapFSNode struct {
	*fs.LoopbackNode
	tapFSNode
}

func (r *loopbackTapFSNode) Access(ctx context.Context, mask uint32) syscall.Errno {
	return syscall.ENOSYS
}

func (r *loopbackTapFSNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	if name == socketName && r.EmbeddedInode().IsRoot() {
		ch := r.GetChild(name)
		if ch != nil {
			return ch, 0
		}
	}
	return r.LoopbackNode.Lookup(ctx, name, out)
}

func (n *loopbackTapFSNode) GetFileInfo(cas CAS) (fi FileInfo, err error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	if n.fileInfo.Digest.Hash != "" {
		return n.fileInfo, nil
	}

	err = func() error {
		path := filepath.Join(n.LoopbackNode.RootData.Path, n.Path(nil))

		var st syscall.Stat_t
		digest, err := n.tapFSData.loopbackHashCache.hashForPath(path, &st)
		if err != nil {
			return err
		}

		typ := FileRegular
		if st.Mode&0111 != 0 {
			typ = FileExecutable
		}
		fi = FileInfo{
			Digest: digest,
			Type:   typ,
		}
		return nil
	}()

	if err != nil {
		return fi, err
	}
	n.fileInfo = fi
	return fi, nil
}

var _ = (fs.NodeWrapChilder)((*loopbackTapFSNode)(nil))

func (n *loopbackTapFSNode) WrapChild(ctx context.Context, ops fs.InodeEmbedder) fs.InodeEmbedder {
	if ln, ok := ops.(*fs.LoopbackNode); ok {
		return &loopbackTapFSNode{
			LoopbackNode: ln,
			tapFSNode: tapFSNode{
				tapFSData: n.tapFSData,
			},
		}
	}

	return ops
}

var _ = (fs.NodeOpener)((*loopbackTapFSNode)(nil))

func (n *loopbackTapFSNode) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	fh, retFlags, errno := n.LoopbackNode.Open(ctx, flags)

	op := OpRead
	if (flags & (syscall.O_APPEND | syscall.O_TRUNC | syscall.O_RDWR | syscall.O_WRONLY)) != 0 {
		op = OpUpdate

		n.mu.Lock()
		n.fileInfo = FileInfo{}
		n.mu.Unlock()
	}
	n.tapFSData.openData(context2pgid(ctx)).record(n.EmbeddedInode(), "", op)
	return fh, retFlags, errno
}

// suppress security attributes, they clutter the logs
func (n *loopbackTapFSNode) Getxattr(ctx context.Context, attr string, dest []byte) (uint32, syscall.Errno) {
	return 0, syscall.ENOSYS
}

var _ = (fs.NodeCreater)((*loopbackTapFSNode)(nil))

func (n *loopbackTapFSNode) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	inode, fh, flags, errno := n.LoopbackNode.Create(ctx, name, flags, mode, out)
	if errno == 0 {
		n.tapFSData.openData(context2pgid(ctx)).record(inode, "", OpCreate)
	}

	return inode, fh, flags, errno
}

func (n *loopbackTapFSNode) Unlink(ctx context.Context, name string) syscall.Errno {
	child := n.GetChild(name)
	errno := n.LoopbackNode.Unlink(ctx, name)
	if errno == 0 {
		n.tapFSData.openData(context2pgid(ctx)).record(child, filepath.Join(n.Path(nil), name), OpDelete)
	}
	return errno
}

func (n *loopbackTapFSNode) Rename(ctx context.Context, name string, newParent fs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	src := n.GetChild(name)
	dest := newParent.EmbeddedInode().GetChild(newName)
	errno := n.LoopbackNode.Rename(ctx, name, newParent, newName, flags)
	if errno == 0 {
		od := n.tapFSData.openData(context2pgid(ctx))
		od.recordRename(src, dest)
	}
	return errno
}

func NewLoopbackTapFS(root *fs.LoopbackNode, data *tapFSData) *loopbackTapFSNode {
	tapRoot := &loopbackTapFSNode{LoopbackNode: root,
		tapFSNode: tapFSNode{
			tapFSData: data,
		},
	}
	return tapRoot
}

func (n *loopbackTapFSNode) fromActionCache(val *ActionCacheValue) error {
	for out, fileInfo := range val.Outputs {
		if err := n.tapFSData.loopbackHashCache.copyTo(
			filepath.Join(n.RootData.Path, out),
			fileInfo.Digest, fileInfo.Type); err != nil {
			return err
		}
	}

	return nil
}

func (n *loopbackTapFSNode) hashForPath(path string) (fi FileInfo, err error) {
	full := filepath.Join(n.RootData.Path, path)
	var st syscall.Stat_t
	if err := syscall.Lstat(full, &st); err != nil {
		return fi, err
	}

	fi.Digest, err = n.tapFSData.loopbackHashCache.hashForPath(full, &st)
	if err != nil {
		return fi, err
	}

	if st.Mode&0111 != 0 {
		fi.Type = FileExecutable
	}
	return fi, err
}
