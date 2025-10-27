package tapfs

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/hanwen/grit/gritfs"
)

func NewGritFSRoot(root *gritfs.RepoNode, data *tapFSData) *repoTapFSNode {
	asTree := &treeTapFSNode{
		&root.TreeNode,
		data,
	}
	return &repoTapFSNode{root, asTree, data}
}

func wrap(ops fs.InodeEmbedder, data *tapFSData) fs.InodeEmbedder {
	switch t := ops.(type) {
	case *gritfs.RepoNode:
		tn := &repoTapFSNode{
			t,
			&treeTapFSNode{&t.TreeNode, data},
			data,
		}
		return tn
	case *gritfs.BlobNode:
		return &blobTapFSNode{
			t, data,
		}
	case *gritfs.TreeNode:
		return &treeTapFSNode{
			t, data,
		}
	}
	return ops
}

var _ fsAPI = (*repoTapFSNode)(nil)

type repoTapFSNode struct {
	*gritfs.RepoNode

	// This is an alias for gritfs.RepoNode.TreeNode
	treeTapFSNode *treeTapFSNode

	tapFSData *tapFSData
}

func (t *repoTapFSNode) WrapChild(_ context.Context, ops fs.InodeEmbedder) fs.InodeEmbedder {
	return wrap(ops, t.tapFSData)
}

func (r *repoTapFSNode) fromActionCache(val *ActionCacheValue) error {
	ctx := context.Background()

	for path, fi := range val.Outputs {
		ch, left := nodeAt(r.EmbeddedInode(), filepath.Dir(path))
		for left != "" && left != "." {
			idx := strings.Index(left, "/")
			if idx < 0 {
				idx = len(left)
			}
			comp := left[:idx]

			n, err := r.NewGitTreeNode()
			if err != nil {
				return err
			}
			in := ch.NewPersistentInode(ctx, n, fs.StableAttr{Mode: syscall.S_IFDIR})
			ch.AddChild(comp, in, true)

			if idx < len(left) {
				left = left[idx+1:]
			} else {
				left = ""
			}
			ch = in
		}

		name := filepath.Base(path)
		if blobChild := ch.GetChild(name); blobChild != nil {
			asBlob, ok := blobChild.Operations().(*blobTapFSNode)
			if !ok {
				return fmt.Errorf("action overwrites %q which is %T", path, ch.Operations())
			}

			before := asBlob.BlobNode.ID()
			after := plumbing.NewHash(fi.Digest.Hash)
			if before != after {
				return fmt.Errorf("action overwrites %q which was %s with %s", path, before, after)
			}

			// file unchanged. next.
			continue
		}

		bn, err := r.NewGitBlobNode(fi.Type.Mode())
		if err != nil {
			return err
		}
		in := ch.NewPersistentInode(ctx, bn, fs.StableAttr{Mode: uint32(fi.Type.Mode())})
		ch.AddChild(name, in, true)

		id := plumbing.NewHash(fi.Digest.Hash)
		bn.SetID(id, fi.Type.Mode(), fi.Digest.Size, time.Now())
	}
	return nil
}

func (r *repoTapFSNode) hashForPath(path string) (FileInfo, error) {
	ch, left := nodeAt(r.EmbeddedInode(), path)
	if left != "" {
		return FileInfo{}, syscall.ENOENT
	}

	switch t := ch.Operations().(type) {
	case *blobTapFSNode:
		bn := t.BlobNode
		var attr fuse.AttrOut
		if errno := t.Getattr(context.Background(), nil, &attr); errno != 0 {
			return FileInfo{}, errno
		}

		return FileInfo{
			Type: FileTypeFromMode(attr.Mode | bn.StableAttr().Mode),
			Digest: Digest{
				Hash: bn.ID().String(),
				Size: attr.Size,
			},
		}, nil
	default:
		return FileInfo{}, fmt.Errorf("type %T not supported", ch.Operations())
	}
}

type blobTapFSNode struct {
	*gritfs.BlobNode
	data *tapFSData
}

type treeTapFSNode struct {
	*gritfs.TreeNode
	tapFSData *tapFSData
}

func (t *treeTapFSNode) WrapChild(_ context.Context, ops fs.InodeEmbedder) fs.InodeEmbedder {
	return wrap(ops, t.tapFSData)
}

var _ = (fs.NodeOpener)((*loopbackTapFSNode)(nil))

func (n *blobTapFSNode) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	fh, retFlags, errno := n.BlobNode.Open(ctx, flags)

	op := OpRead
	if (flags & (syscall.O_APPEND | syscall.O_TRUNC | syscall.O_RDWR | syscall.O_WRONLY)) != 0 {
		op = OpUpdate
	}
	n.data.openData(context2pgid(ctx)).record(n.EmbeddedInode(), "", op)
	return fh, retFlags, errno
}

// suppress security attributes, they clutter the logs
func (n *blobTapFSNode) Getxattr(ctx context.Context, attr string, dest []byte) (uint32, syscall.Errno) {
	return 0, syscall.ENOSYS
}

var _ = (fs.NodeCreater)((*treeTapFSNode)(nil))

func (n *repoTapFSNode) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	return n.treeTapFSNode.Create(ctx, name, flags, mode, out)
}

func (n *treeTapFSNode) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	inode, fh, flags, errno := n.TreeNode.Create(ctx, name, flags, mode, out)
	if errno == 0 {
		n.tapFSData.openData(context2pgid(ctx)).record(inode, "", OpCreate)
	}

	return inode, fh, flags, errno
}

func (n *repoTapFSNode) Unlink(ctx context.Context, name string) syscall.Errno {
	return n.treeTapFSNode.Unlink(ctx, name)
}

func (n *treeTapFSNode) Unlink(ctx context.Context, name string) syscall.Errno {
	child := n.GetChild(name)
	errno := n.TreeNode.Unlink(ctx, name)
	if errno == 0 {
		n.tapFSData.openData(context2pgid(ctx)).record(child, filepath.Join(n.Path(nil), name), OpDelete)
	}
	return errno
}

// TODO
/*
func (n *treeTapFSNode) xRename(ctx context.Context, name string, newParent fs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	src := n.GetChild(name)
	dest := newParent.EmbeddedInode().GetChild(newName)
	errno := n.TreeNode.Rename(ctx, name, newParent, newName, flags)
	if errno == 0 {
		od := n.tapFSData.openData(context2pgid(ctx))
		od.recordRename(src, dest)
	}
	return errno
}
*/
