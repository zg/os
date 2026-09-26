package filesystem

import (
	"fmt"
	"io"
	"time"
)

// Open flags.
const (
	O_RDONLY = 0
	O_WRONLY = 1
	O_RDWR   = 2
	O_CREATE = 1 << 6
	O_TRUNC  = 1 << 9
	O_APPEND = 1 << 10
)

// Open returns a file descriptor for a path.
//
// Descriptors are per-filesystem integers, deliberately mirroring the Unix
// model: the offset lives in the open file description, not in the inode,
// so two descriptors on one file have independent positions.
func (fs *FS) Open(p string, flags int) (int, error) {
	node, err := fs.resolve(p)
	if err != nil {
		if flags&O_CREATE == 0 {
			return 0, err
		}
		if err := fs.Create(p, 0o644); err != nil {
			return 0, err
		}
		node, err = fs.resolve(p)
		if err != nil {
			return 0, err
		}
	}

	if node.Type == TypeDir && flags&(O_WRONLY|O_RDWR) != 0 {
		return 0, fmt.Errorf("%w: %q", ErrIsDir, p)
	}
	if flags&O_TRUNC != 0 && node.Type == TypeFile {
		fs.freeBlocks(node.Blocks)
		node.Blocks = nil
		node.Size = 0
		node.ModTime = time.Now()
	}

	fd := fs.nextFD
	fs.nextFD++
	f := &openFile{inode: node.Number, append: flags&O_APPEND != 0}
	if f.append {
		f.offset = node.Size
	}
	fs.openFiles[fd] = f
	return fd, nil
}

// Close releases a descriptor.
func (fs *FS) Close(fd int) error {
	if _, ok := fs.openFiles[fd]; !ok {
		return fmt.Errorf("%w: %d", ErrInvalidFD, fd)
	}
	delete(fs.openFiles, fd)
	return nil
}

// Read reads from a descriptor at its current offset, advancing it.
func (fs *FS) Read(fd int, buf []byte) (int, error) {
	f, ok := fs.openFiles[fd]
	if !ok {
		return 0, fmt.Errorf("%w: %d", ErrInvalidFD, fd)
	}
	node := fs.inodes[f.inode]
	if node == nil {
		return 0, ErrNotFound
	}
	if f.offset >= node.Size {
		return 0, io.EOF
	}

	n := 0
	for n < len(buf) && f.offset < node.Size {
		blockIdx := f.offset / BlockSize
		if blockIdx >= len(node.Blocks) {
			break
		}
		within := f.offset % BlockSize
		// Copy the smallest of: what is left in this block, what is left
		// in the file, and what is left in the buffer.
		avail := min(BlockSize-within, node.Size-f.offset, len(buf)-n)
		copy(buf[n:], fs.blocks[node.Blocks[blockIdx]][within:within+avail])
		n += avail
		f.offset += avail
	}
	return n, nil
}

// Write writes to a descriptor at its current offset, growing the file and
// allocating blocks as needed.
func (fs *FS) Write(fd int, data []byte) (int, error) {
	f, ok := fs.openFiles[fd]
	if !ok {
		return 0, fmt.Errorf("%w: %d", ErrInvalidFD, fd)
	}
	node := fs.inodes[f.inode]
	if node == nil {
		return 0, ErrNotFound
	}
	if node.Type == TypeDir {
		return 0, ErrIsDir
	}

	// Appending always writes at the current end, even if another
	// descriptor has extended the file since this one was opened.
	if f.append {
		f.offset = node.Size
	}

	n := 0
	for n < len(data) {
		blockIdx := f.offset / BlockSize
		for blockIdx >= len(node.Blocks) {
			b, err := fs.allocBlock()
			if err != nil {
				// Return what was written rather than discarding it: a
				// short write is the honest outcome when space runs out.
				if n > 0 {
					fs.updateSize(node, f.offset)
				}
				return n, err
			}
			node.Blocks = append(node.Blocks, b)
		}
		within := f.offset % BlockSize
		avail := min(BlockSize-within, len(data)-n)
		copy(fs.blocks[node.Blocks[blockIdx]][within:], data[n:n+avail])
		n += avail
		f.offset += avail
	}

	fs.updateSize(node, f.offset)
	return n, nil
}

// updateSize grows a file's recorded size; writes in the middle of a file
// must not shrink it.
func (fs *FS) updateSize(node *Inode, offset int) {
	if offset > node.Size {
		node.Size = offset
	}
	node.ModTime = time.Now()
}

// Seek sets a descriptor's offset.
func (fs *FS) Seek(fd, offset, whence int) (int, error) {
	f, ok := fs.openFiles[fd]
	if !ok {
		return 0, fmt.Errorf("%w: %d", ErrInvalidFD, fd)
	}
	node := fs.inodes[f.inode]

	var abs int
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = f.offset + offset
	case io.SeekEnd:
		abs = node.Size + offset
	default:
		return 0, fmt.Errorf("fs: invalid whence %d", whence)
	}
	if abs < 0 {
		return 0, fmt.Errorf("fs: negative seek position %d", abs)
	}
	f.offset = abs
	return abs, nil
}

// Tell returns a descriptor's current offset.
func (fs *FS) Tell(fd int) (int, error) {
	f, ok := fs.openFiles[fd]
	if !ok {
		return 0, fmt.Errorf("%w: %d", ErrInvalidFD, fd)
	}
	return f.offset, nil
}

// OpenCount returns how many descriptors are open, for leak checks.
func (fs *FS) OpenCount() int { return len(fs.openFiles) }
