package main

import (
	"context"
	"os"
	"syscall"

	"github.com/hanwen/go-fuse/fs"
	"github.com/hanwen/go-fuse/fuse"
)

func (fn *remoteFileNode) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Mode = 0644
	out.Uid = uint32(os.Getuid())
	out.Gid = uint32(os.Getgid())
	out.Atime = fn.metadata.LastModified
	out.Mtime = fn.metadata.LastModified
	out.Ctime = fn.metadata.LastModified
	out.Size = fn.metadata.Size
	const bs = 512
	// out.Blksize = bs
	out.Blocks = (out.Size + bs - 1) / bs
	return 0
}
