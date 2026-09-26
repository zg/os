// Package filesystem implements an inode-based filesystem over a simulated
// block device.
//
// The structure mirrors a classic Unix filesystem: a fixed pool of inodes,
// each holding metadata and a list of data blocks; directories are files
// whose contents are name-to-inode mappings; and a free block bitmap tracks
// allocation. Hard links fall out of this naturally — a directory entry is
// just a name pointing at an inode number.
package filesystem

import (
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"
)

// BlockSize is the bytes per block.
const BlockSize = 512

// Errors returned by filesystem operations.
var (
	ErrNotFound   = errors.New("fs: no such file or directory")
	ErrExists     = errors.New("fs: file exists")
	ErrNotDir     = errors.New("fs: not a directory")
	ErrIsDir      = errors.New("fs: is a directory")
	ErrNotEmpty   = errors.New("fs: directory not empty")
	ErrNoSpace    = errors.New("fs: no space left on device")
	ErrNoInodes   = errors.New("fs: no free inodes")
	ErrBadPath    = errors.New("fs: invalid path")
	ErrPermission = errors.New("fs: permission denied")
	ErrInvalidFD  = errors.New("fs: bad file descriptor")
)

// FileType distinguishes files from directories.
type FileType uint8

const (
	TypeFile FileType = iota
	TypeDir
)

// Inode holds a file's metadata and block list.
type Inode struct {
	Number int
	Type   FileType
	Size   int
	Mode   uint16
	// Links counts directory entries pointing here. The inode is freed
	// only when this reaches zero — which is what makes hard links work.
	Links   int
	Blocks  []int
	ModTime time.Time
	// Entries is the directory contents, for directory inodes.
	Entries map[string]int
}

// FS is a filesystem.
type FS struct {
	inodes    []*Inode
	blocks    [][]byte
	freeBlock []bool
	root      int
	// openFiles maps descriptors to open file state.
	openFiles map[int]*openFile
	nextFD    int
}

type openFile struct {
	inode  int
	offset int
	append bool
}

// New returns a formatted filesystem with the given capacity.
func New(numInodes, numBlocks int) *FS {
	if numInodes < 2 {
		numInodes = 2
	}
	if numBlocks < 1 {
		numBlocks = 1
	}

	fs := &FS{
		inodes:    make([]*Inode, numInodes),
		blocks:    make([][]byte, numBlocks),
		freeBlock: make([]bool, numBlocks),
		openFiles: map[int]*openFile{},
		nextFD:    3, // 0, 1 and 2 are reserved for the standard streams
	}
	for i := range fs.freeBlock {
		fs.freeBlock[i] = true
	}

	// Inode 0 is the root directory. Its link count starts at 2: one for
	// its name in the parent, one for its own ".".
	root := &Inode{
		Number: 0, Type: TypeDir, Mode: 0o755, Links: 2,
		Entries: map[string]int{}, ModTime: time.Now(),
	}
	fs.inodes[0] = root
	fs.root = 0
	return fs
}

// allocInode reserves a free inode.
func (fs *FS) allocInode(t FileType, mode uint16) (*Inode, error) {
	for i, in := range fs.inodes {
		if in != nil {
			continue
		}
		node := &Inode{Number: i, Type: t, Mode: mode, Links: 1, ModTime: time.Now()}
		if t == TypeDir {
			node.Entries = map[string]int{}
		}
		fs.inodes[i] = node
		return node, nil
	}
	return nil, ErrNoInodes
}

// allocBlock reserves a free data block.
func (fs *FS) allocBlock() (int, error) {
	for i, free := range fs.freeBlock {
		if !free {
			continue
		}
		fs.freeBlock[i] = false
		fs.blocks[i] = make([]byte, BlockSize)
		return i, nil
	}
	return 0, ErrNoSpace
}

func (fs *FS) freeBlocks(blocks []int) {
	for _, b := range blocks {
		fs.freeBlock[b] = true
		fs.blocks[b] = nil
	}
}

// splitPath breaks an absolute path into components, rejecting anything
// that is not absolute or that tries to escape the root.
func splitPath(p string) ([]string, error) {
	if !strings.HasPrefix(p, "/") {
		return nil, fmt.Errorf("%w: %q is not absolute", ErrBadPath, p)
	}
	cleaned := path.Clean(p)
	if cleaned == "/" {
		return nil, nil
	}
	parts := strings.Split(strings.TrimPrefix(cleaned, "/"), "/")
	for _, c := range parts {
		// path.Clean resolves "..", but a leading one would remain and
		// would escape the root.
		if c == ".." || c == "" {
			return nil, fmt.Errorf("%w: %q escapes the root", ErrBadPath, p)
		}
	}
	return parts, nil
}

// resolve walks a path to its inode.
func (fs *FS) resolve(p string) (*Inode, error) {
	parts, err := splitPath(p)
	if err != nil {
		return nil, err
	}
	node := fs.inodes[fs.root]
	for _, name := range parts {
		if node.Type != TypeDir {
			return nil, fmt.Errorf("%w: %q", ErrNotDir, p)
		}
		num, ok := node.Entries[name]
		if !ok {
			return nil, fmt.Errorf("%w: %q", ErrNotFound, p)
		}
		node = fs.inodes[num]
		if node == nil {
			return nil, fmt.Errorf("%w: %q", ErrNotFound, p)
		}
	}
	return node, nil
}

// resolveParent returns the parent directory of a path and the final name.
func (fs *FS) resolveParent(p string) (*Inode, string, error) {
	parts, err := splitPath(p)
	if err != nil {
		return nil, "", err
	}
	if len(parts) == 0 {
		return nil, "", fmt.Errorf("%w: cannot operate on the root", ErrBadPath)
	}

	dir := fs.inodes[fs.root]
	for _, name := range parts[:len(parts)-1] {
		if dir.Type != TypeDir {
			return nil, "", fmt.Errorf("%w: %q", ErrNotDir, p)
		}
		num, ok := dir.Entries[name]
		if !ok {
			return nil, "", fmt.Errorf("%w: %q", ErrNotFound, p)
		}
		dir = fs.inodes[num]
	}
	if dir.Type != TypeDir {
		return nil, "", fmt.Errorf("%w: %q", ErrNotDir, p)
	}
	return dir, parts[len(parts)-1], nil
}

// Mkdir creates a directory.
func (fs *FS) Mkdir(p string, mode uint16) error {
	parent, name, err := fs.resolveParent(p)
	if err != nil {
		return err
	}
	if _, exists := parent.Entries[name]; exists {
		return fmt.Errorf("%w: %q", ErrExists, p)
	}

	node, err := fs.allocInode(TypeDir, mode)
	if err != nil {
		return err
	}
	// A new directory has two links: its name in the parent, and its ".".
	node.Links = 2
	parent.Entries[name] = node.Number
	// The parent gains a link too, from the new directory's "..".
	parent.Links++
	parent.ModTime = time.Now()
	return nil
}

// MkdirAll creates a directory and any missing parents.
func (fs *FS) MkdirAll(p string, mode uint16) error {
	parts, err := splitPath(p)
	if err != nil {
		return err
	}
	cur := "/"
	for _, name := range parts {
		cur = path.Join(cur, name)
		if node, err := fs.resolve(cur); err == nil {
			if node.Type != TypeDir {
				return fmt.Errorf("%w: %q", ErrNotDir, cur)
			}
			continue
		}
		if err := fs.Mkdir(cur, mode); err != nil {
			return err
		}
	}
	return nil
}

// Create makes an empty file, truncating it if it already exists.
func (fs *FS) Create(p string, mode uint16) error {
	parent, name, err := fs.resolveParent(p)
	if err != nil {
		return err
	}
	if num, exists := parent.Entries[name]; exists {
		existing := fs.inodes[num]
		if existing.Type == TypeDir {
			return fmt.Errorf("%w: %q", ErrIsDir, p)
		}
		fs.freeBlocks(existing.Blocks)
		existing.Blocks = nil
		existing.Size = 0
		existing.ModTime = time.Now()
		return nil
	}

	node, err := fs.allocInode(TypeFile, mode)
	if err != nil {
		return err
	}
	parent.Entries[name] = node.Number
	parent.ModTime = time.Now()
	return nil
}

// WriteFile writes data to a path, creating or truncating the file.
func (fs *FS) WriteFile(p string, data []byte, mode uint16) error {
	if err := fs.Create(p, mode); err != nil {
		return err
	}
	node, err := fs.resolve(p)
	if err != nil {
		return err
	}

	needed := (len(data) + BlockSize - 1) / BlockSize
	// Allocate everything up front: a partial write that then runs out of
	// space would leave the file inconsistent.
	blocks := make([]int, 0, needed)
	for range needed {
		b, err := fs.allocBlock()
		if err != nil {
			fs.freeBlocks(blocks) // roll back
			return err
		}
		blocks = append(blocks, b)
	}

	for i, b := range blocks {
		start := i * BlockSize
		end := min(start+BlockSize, len(data))
		copy(fs.blocks[b], data[start:end])
	}

	node.Blocks = blocks
	node.Size = len(data)
	node.ModTime = time.Now()
	return nil
}

// ReadFile returns a file's contents.
func (fs *FS) ReadFile(p string) ([]byte, error) {
	node, err := fs.resolve(p)
	if err != nil {
		return nil, err
	}
	if node.Type == TypeDir {
		return nil, fmt.Errorf("%w: %q", ErrIsDir, p)
	}

	data := make([]byte, 0, node.Size)
	remaining := node.Size
	for _, b := range node.Blocks {
		n := min(BlockSize, remaining)
		data = append(data, fs.blocks[b][:n]...)
		remaining -= n
	}
	return data, nil
}

// Remove deletes a file or an empty directory.
func (fs *FS) Remove(p string) error {
	parent, name, err := fs.resolveParent(p)
	if err != nil {
		return err
	}
	num, ok := parent.Entries[name]
	if !ok {
		return fmt.Errorf("%w: %q", ErrNotFound, p)
	}
	node := fs.inodes[num]

	if node.Type == TypeDir {
		if len(node.Entries) > 0 {
			return fmt.Errorf("%w: %q", ErrNotEmpty, p)
		}
		parent.Links--
	}

	delete(parent.Entries, name)
	parent.ModTime = time.Now()

	// The inode survives until its last link goes; that is what lets a
	// hard-linked file outlive one of its names.
	node.Links--
	if node.Links <= 0 {
		fs.freeBlocks(node.Blocks)
		fs.inodes[num] = nil
	}
	return nil
}

// Link creates a hard link: a second name for the same inode.
func (fs *FS) Link(oldPath, newPath string) error {
	node, err := fs.resolve(oldPath)
	if err != nil {
		return err
	}
	if node.Type == TypeDir {
		// Linking directories would allow cycles in the tree.
		return fmt.Errorf("%w: cannot hard link a directory", ErrIsDir)
	}

	parent, name, err := fs.resolveParent(newPath)
	if err != nil {
		return err
	}
	if _, exists := parent.Entries[name]; exists {
		return fmt.Errorf("%w: %q", ErrExists, newPath)
	}

	parent.Entries[name] = node.Number
	node.Links++
	parent.ModTime = time.Now()
	return nil
}

// Rename moves a file or directory.
func (fs *FS) Rename(oldPath, newPath string) error {
	oldParent, oldName, err := fs.resolveParent(oldPath)
	if err != nil {
		return err
	}
	num, ok := oldParent.Entries[oldName]
	if !ok {
		return fmt.Errorf("%w: %q", ErrNotFound, oldPath)
	}

	newParent, newName, err := fs.resolveParent(newPath)
	if err != nil {
		return err
	}
	if _, exists := newParent.Entries[newName]; exists {
		return fmt.Errorf("%w: %q", ErrExists, newPath)
	}

	newParent.Entries[newName] = num
	delete(oldParent.Entries, oldName)

	// Moving a directory between parents transfers its ".." link.
	if fs.inodes[num].Type == TypeDir && oldParent != newParent {
		oldParent.Links--
		newParent.Links++
	}
	oldParent.ModTime = time.Now()
	newParent.ModTime = time.Now()
	return nil
}

// DirEntry is one entry in a directory listing.
type DirEntry struct {
	Name  string
	Inode int
	Type  FileType
	Size  int
	Links int
}

// ReadDir lists a directory, sorted by name.
func (fs *FS) ReadDir(p string) ([]DirEntry, error) {
	node, err := fs.resolve(p)
	if err != nil {
		return nil, err
	}
	if node.Type != TypeDir {
		return nil, fmt.Errorf("%w: %q", ErrNotDir, p)
	}

	out := make([]DirEntry, 0, len(node.Entries))
	for name, num := range node.Entries {
		child := fs.inodes[num]
		out = append(out, DirEntry{
			Name: name, Inode: num, Type: child.Type,
			Size: child.Size, Links: child.Links,
		})
	}
	// Sorting keeps listings deterministic; Go randomises map iteration.
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Stat returns a path's inode.
func (fs *FS) Stat(p string) (*Inode, error) { return fs.resolve(p) }

// Exists reports whether a path resolves.
func (fs *FS) Exists(p string) bool {
	_, err := fs.resolve(p)
	return err == nil
}

// Statfs describes filesystem usage.
type Statfs struct {
	TotalBlocks, FreeBlocks int
	TotalInodes, FreeInodes int
}

// UsedBlocks returns the number of allocated blocks.
func (s Statfs) UsedBlocks() int { return s.TotalBlocks - s.FreeBlocks }

// Statfs reports capacity and usage.
func (fs *FS) Statfs() Statfs {
	s := Statfs{TotalBlocks: len(fs.blocks), TotalInodes: len(fs.inodes)}
	for _, free := range fs.freeBlock {
		if free {
			s.FreeBlocks++
		}
	}
	for _, in := range fs.inodes {
		if in == nil {
			s.FreeInodes++
		}
	}
	return s
}
