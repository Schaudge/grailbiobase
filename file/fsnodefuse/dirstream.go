package fsnodefuse

import (
	"context"
	"crypto/sha512"
	"encoding/binary"
	"fmt"
	"io"
	"syscall"

	"github.com/grailbio/base/file/fsnode"
	"github.com/grailbio/base/log"
	"github.com/grailbio/base/writehash"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

type dirStream struct {
	ctx        context.Context
	parentIno  uint64
	iter       fsnode.Iterator
	eof        bool
	current    fsnode.T
	currentErr error
}

func newDirStream(ctx context.Context, parentIno uint64, iter fsnode.Iterator) *dirStream {
	return &dirStream{
		ctx:       ctx,
		parentIno: parentIno,
		iter:      iter,
	}
}

func (d *dirStream) HasNext() bool {
	if d.current != nil || d.currentErr != nil {
		return true
	}
	if d.eof {
		return false
	}
	next, err := d.iter.Next(d.ctx)
	if err == io.EOF {
		d.eof = true
		return false
	} else if err != nil {
		d.currentErr = fmt.Errorf("fsnodefuse.dirStream: %w", err)
		return false
	}
	d.current = next
	return true
}

func (d *dirStream) Next() (fuse.DirEntry, syscall.Errno) {
	if err := d.currentErr; err != nil {
		return fuse.DirEntry{}, errToErrno(err)
	}
	current := d.current
	d.current = nil
	return fsEntryToFuseEntry(d.parentIno, current.Name(), current.IsDir()), fs.OK
}

func (d *dirStream) Close() {
	if err := d.iter.Close(d.ctx); err != nil {
		log.Error.Printf("fsnodefuse.dirStream: error on close: %v", err)
	}
	d.iter = nil
	d.current = nil
}

func fsEntryToFuseEntry(parentIno uint64, name string, isDir bool) fuse.DirEntry {
	fe := fuse.DirEntry{
		Name: name,
		Ino:  hashParentInoAndName(parentIno, name),
	}
	if isDir {
		fe.Mode |= syscall.S_IFDIR
	} else {
		fe.Mode |= syscall.S_IFREG
	}
	return fe
}

func hashParentInoAndName(parentIno uint64, name string) uint64 {
	h := sha512.New()
	writehash.Uint64(h, parentIno)
	writehash.String(h, name)
	return binary.LittleEndian.Uint64(h.Sum(nil)[:8])

}

func hashIno(parent fs.InodeEmbedder, name string) uint64 {
	return hashParentInoAndName(parent.EmbeddedInode().StableAttr().Ino, name)
}
