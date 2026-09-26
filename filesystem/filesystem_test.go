package filesystem

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func newFS() *FS { return New(32, 128) }

func TestCreateAndRead(t *testing.T) {
	fs := newFS()
	data := []byte("hello, filesystem")

	if err := fs.WriteFile("/hello.txt", data, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := fs.ReadFile("/hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("= %q, want %q", got, data)
	}
}

// A file larger than one block must span blocks correctly.
func TestMultiBlockFile(t *testing.T) {
	fs := newFS()
	data := bytes.Repeat([]byte("abcdefgh"), BlockSize/4) // 2 blocks' worth

	if err := fs.WriteFile("/big.bin", data, 0o644); err != nil {
		t.Fatal(err)
	}
	node, err := fs.Stat("/big.bin")
	if err != nil {
		t.Fatal(err)
	}
	if len(node.Blocks) < 2 {
		t.Errorf("file uses %d blocks, want at least 2", len(node.Blocks))
	}

	got, err := fs.ReadFile("/big.bin")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("multi-block round trip corrupted the data (%d vs %d bytes)", len(got), len(data))
	}
}

func TestDirectories(t *testing.T) {
	fs := newFS()
	if err := fs.Mkdir("/dir", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := fs.WriteFile("/dir/file.txt", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	entries, err := fs.ReadDir("/dir")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name != "file.txt" {
		t.Errorf("entries = %+v", entries)
	}
}

func TestMkdirAll(t *testing.T) {
	fs := newFS()
	if err := fs.MkdirAll("/a/b/c/d", 0o755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/a", "/a/b", "/a/b/c", "/a/b/c/d"} {
		if !fs.Exists(p) {
			t.Errorf("%s was not created", p)
		}
	}
	// Repeating is not an error.
	if err := fs.MkdirAll("/a/b/c/d", 0o755); err != nil {
		t.Errorf("MkdirAll should be idempotent: %v", err)
	}
}

// Listings must be sorted, or they would vary run to run with Go's map
// iteration.
func TestReadDirIsSorted(t *testing.T) {
	fs := newFS()
	for _, name := range []string{"zebra", "apple", "mango", "banana"} {
		fs.WriteFile("/"+name, []byte("x"), 0o644)
	}

	first, err := fs.ReadDir("/")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"apple", "banana", "mango", "zebra"}
	for i, w := range want {
		if first[i].Name != w {
			t.Errorf("entry %d = %s, want %s", i, first[i].Name, w)
		}
	}
	// And stable across repeated calls.
	for range 20 {
		again, _ := fs.ReadDir("/")
		for i := range again {
			if again[i].Name != first[i].Name {
				t.Fatal("directory listing order varies between calls")
			}
		}
	}
}

// A hard link is a second name for one inode: the data outlives the removal
// of either name, and the link count is what decides when it is freed.
func TestHardLinks(t *testing.T) {
	fs := newFS()
	data := []byte("shared content")
	fs.WriteFile("/original.txt", data, 0o644)

	if err := fs.Link("/original.txt", "/link.txt"); err != nil {
		t.Fatal(err)
	}

	node, _ := fs.Stat("/original.txt")
	linked, _ := fs.Stat("/link.txt")
	if node.Number != linked.Number {
		t.Errorf("link points at inode %d, original is %d", linked.Number, node.Number)
	}
	if node.Links != 2 {
		t.Errorf("link count = %d, want 2", node.Links)
	}

	// Removing one name leaves the data reachable through the other.
	if err := fs.Remove("/original.txt"); err != nil {
		t.Fatal(err)
	}
	got, err := fs.ReadFile("/link.txt")
	if err != nil {
		t.Fatalf("data should survive removal of the other name: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("= %q, want %q", got, data)
	}
	if n, _ := fs.Stat("/link.txt"); n.Links != 1 {
		t.Errorf("link count = %d, want 1", n.Links)
	}
}

// Removing the last link must free the blocks, or the filesystem leaks.
func TestRemoveFreesBlocks(t *testing.T) {
	fs := newFS()
	before := fs.Statfs().FreeBlocks

	fs.WriteFile("/temp.bin", bytes.Repeat([]byte("x"), BlockSize*3), 0o644)
	if during := fs.Statfs().FreeBlocks; during >= before {
		t.Fatal("writing did not consume blocks")
	}

	if err := fs.Remove("/temp.bin"); err != nil {
		t.Fatal(err)
	}
	if after := fs.Statfs().FreeBlocks; after != before {
		t.Errorf("free blocks = %d after removal, want %d — blocks leaked", after, before)
	}
}

// Blocks must not be freed while another link still refers to the inode.
func TestRemoveWithRemainingLinkKeepsBlocks(t *testing.T) {
	fs := newFS()
	fs.WriteFile("/a.txt", bytes.Repeat([]byte("x"), BlockSize*2), 0o644)
	fs.Link("/a.txt", "/b.txt")

	before := fs.Statfs().FreeBlocks
	fs.Remove("/a.txt")
	if after := fs.Statfs().FreeBlocks; after != before {
		t.Errorf("blocks were freed while a link remained: %d -> %d", before, after)
	}
}

func TestRemoveNonEmptyDirectoryFails(t *testing.T) {
	fs := newFS()
	fs.Mkdir("/dir", 0o755)
	fs.WriteFile("/dir/file", []byte("x"), 0o644)

	if err := fs.Remove("/dir"); !errors.Is(err, ErrNotEmpty) {
		t.Errorf("err = %v, want ErrNotEmpty", err)
	}
	// It works once emptied.
	fs.Remove("/dir/file")
	if err := fs.Remove("/dir"); err != nil {
		t.Errorf("removing an empty directory failed: %v", err)
	}
}

func TestRename(t *testing.T) {
	fs := newFS()
	fs.WriteFile("/old.txt", []byte("content"), 0o644)
	fs.Mkdir("/dir", 0o755)

	if err := fs.Rename("/old.txt", "/dir/new.txt"); err != nil {
		t.Fatal(err)
	}
	if fs.Exists("/old.txt") {
		t.Error("the old name still resolves")
	}
	got, err := fs.ReadFile("/dir/new.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "content" {
		t.Errorf("= %q", got)
	}
}

// Paths must be absolute and must not escape the root.
func TestPathValidation(t *testing.T) {
	fs := newFS()
	fs.Mkdir("/dir", 0o755)

	bad := []string{
		"relative/path",
		"",
		"/../escaped",
		"/dir/../../escaped",
	}
	for _, p := range bad {
		t.Run(p, func(t *testing.T) {
			if _, err := fs.Stat(p); err == nil {
				t.Errorf("%q should not resolve", p)
			}
		})
	}

	// path.Clean resolves an interior "..", which is legitimate.
	fs.WriteFile("/dir/f.txt", []byte("x"), 0o644)
	if _, err := fs.Stat("/dir/../dir/f.txt"); err != nil {
		t.Errorf("an interior .. that stays in the tree should resolve: %v", err)
	}
}

func TestErrors(t *testing.T) {
	fs := newFS()
	fs.Mkdir("/dir", 0o755)
	fs.WriteFile("/file.txt", []byte("x"), 0o644)

	if _, err := fs.ReadFile("/nonexistent"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
	if _, err := fs.ReadFile("/dir"); !errors.Is(err, ErrIsDir) {
		t.Errorf("err = %v, want ErrIsDir", err)
	}
	if _, err := fs.ReadDir("/file.txt"); !errors.Is(err, ErrNotDir) {
		t.Errorf("err = %v, want ErrNotDir", err)
	}
	if err := fs.Mkdir("/dir", 0o755); !errors.Is(err, ErrExists) {
		t.Errorf("err = %v, want ErrExists", err)
	}
	if err := fs.Mkdir("/file.txt/sub", 0o755); err == nil {
		t.Error("creating under a file should fail")
	}
	if err := fs.Link("/dir", "/dirlink"); !errors.Is(err, ErrIsDir) {
		t.Errorf("hard-linking a directory should fail: %v", err)
	}
}

// Running out of space must be reported, and must not leave a partially
// written file holding blocks.
func TestNoSpaceRollsBack(t *testing.T) {
	fs := New(32, 4) // only 4 blocks

	// More data than fits.
	err := fs.WriteFile("/toobig.bin", bytes.Repeat([]byte("x"), BlockSize*10), 0o644)
	if !errors.Is(err, ErrNoSpace) {
		t.Fatalf("err = %v, want ErrNoSpace", err)
	}
	// The partially allocated blocks must have been returned.
	if got := fs.Statfs().FreeBlocks; got != 4 {
		t.Errorf("free blocks = %d after a failed write, want 4 — allocation was not rolled back", got)
	}
}

func TestNoFreeInodes(t *testing.T) {
	fs := New(3, 64) // root plus two
	fs.WriteFile("/a", []byte("x"), 0o644)
	fs.WriteFile("/b", []byte("x"), 0o644)
	if err := fs.WriteFile("/c", []byte("x"), 0o644); !errors.Is(err, ErrNoInodes) {
		t.Errorf("err = %v, want ErrNoInodes", err)
	}
}

func TestOpenReadWriteSeek(t *testing.T) {
	fs := newFS()
	fd, err := fs.Open("/f.txt", O_CREATE|O_RDWR)
	if err != nil {
		t.Fatal(err)
	}

	data := []byte("hello world")
	n, err := fs.Write(fd, data)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(data) {
		t.Errorf("wrote %d bytes, want %d", n, len(data))
	}

	if _, err := fs.Seek(fd, 0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len(data))
	if _, err := fs.Read(fd, buf); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf, data) {
		t.Errorf("= %q, want %q", buf, data)
	}

	// Seeking from the end.
	pos, err := fs.Seek(fd, -5, io.SeekEnd)
	if err != nil {
		t.Fatal(err)
	}
	if pos != len(data)-5 {
		t.Errorf("position = %d, want %d", pos, len(data)-5)
	}

	if err := fs.Close(fd); err != nil {
		t.Fatal(err)
	}
	if fs.OpenCount() != 0 {
		t.Errorf("%d descriptors still open", fs.OpenCount())
	}
}

// Each descriptor has its own offset: that is the point of putting it in
// the open file description rather than the inode.
func TestDescriptorsHaveIndependentOffsets(t *testing.T) {
	fs := newFS()
	fs.WriteFile("/f.txt", []byte("abcdefghij"), 0o644)

	a, _ := fs.Open("/f.txt", O_RDONLY)
	b, _ := fs.Open("/f.txt", O_RDONLY)
	defer fs.Close(a)
	defer fs.Close(b)

	buf := make([]byte, 5)
	fs.Read(a, buf)
	if string(buf) != "abcde" {
		t.Errorf("first descriptor read %q", buf)
	}

	// The second descriptor must still be at the start.
	fs.Read(b, buf)
	if string(buf) != "abcde" {
		t.Errorf("second descriptor read %q; offsets are shared", buf)
	}
}

func TestAppendMode(t *testing.T) {
	fs := newFS()
	fs.WriteFile("/log.txt", []byte("first\n"), 0o644)

	fd, err := fs.Open("/log.txt", O_WRONLY|O_APPEND)
	if err != nil {
		t.Fatal(err)
	}
	fs.Write(fd, []byte("second\n"))
	fs.Close(fd)

	got, _ := fs.ReadFile("/log.txt")
	if string(got) != "first\nsecond\n" {
		t.Errorf("= %q, want %q", got, "first\nsecond\n")
	}
}

func TestTruncateOnOpen(t *testing.T) {
	fs := newFS()
	fs.WriteFile("/f.txt", []byte("original content"), 0o644)

	fd, err := fs.Open("/f.txt", O_WRONLY|O_TRUNC)
	if err != nil {
		t.Fatal(err)
	}
	fs.Write(fd, []byte("new"))
	fs.Close(fd)

	got, _ := fs.ReadFile("/f.txt")
	if string(got) != "new" {
		t.Errorf("= %q, want %q", got, "new")
	}
}

func TestReadAtEOF(t *testing.T) {
	fs := newFS()
	fs.WriteFile("/f.txt", []byte("abc"), 0o644)
	fd, _ := fs.Open("/f.txt", O_RDONLY)
	defer fs.Close(fd)

	buf := make([]byte, 10)
	n, _ := fs.Read(fd, buf)
	if n != 3 {
		t.Errorf("read %d bytes, want 3", n)
	}
	// A second read is at EOF.
	if _, err := fs.Read(fd, buf); !errors.Is(err, io.EOF) {
		t.Errorf("err = %v, want EOF", err)
	}
}

func TestInvalidDescriptor(t *testing.T) {
	fs := newFS()
	buf := make([]byte, 10)
	if _, err := fs.Read(999, buf); !errors.Is(err, ErrInvalidFD) {
		t.Errorf("err = %v, want ErrInvalidFD", err)
	}
	if _, err := fs.Write(999, buf); !errors.Is(err, ErrInvalidFD) {
		t.Errorf("err = %v, want ErrInvalidFD", err)
	}
	if err := fs.Close(999); !errors.Is(err, ErrInvalidFD) {
		t.Errorf("err = %v, want ErrInvalidFD", err)
	}
}

// Writing into the middle of a file must not shrink it.
func TestWriteInMiddleDoesNotTruncate(t *testing.T) {
	fs := newFS()
	fs.WriteFile("/f.txt", []byte("0123456789"), 0o644)

	fd, _ := fs.Open("/f.txt", O_RDWR)
	fs.Seek(fd, 3, io.SeekStart)
	fs.Write(fd, []byte("XX"))
	fs.Close(fd)

	got, _ := fs.ReadFile("/f.txt")
	if string(got) != "012XX56789" {
		t.Errorf("= %q, want %q", got, "012XX56789")
	}
}

func TestStatfs(t *testing.T) {
	fs := New(10, 20)
	s := fs.Statfs()
	if s.TotalInodes != 10 || s.TotalBlocks != 20 {
		t.Errorf("totals = %d inodes, %d blocks", s.TotalInodes, s.TotalBlocks)
	}
	// Root occupies one inode.
	if s.FreeInodes != 9 {
		t.Errorf("free inodes = %d, want 9", s.FreeInodes)
	}
	if s.UsedBlocks() != 0 {
		t.Errorf("used blocks = %d, want 0", s.UsedBlocks())
	}

	fs.WriteFile("/f", bytes.Repeat([]byte("x"), BlockSize), 0o644)
	if got := fs.Statfs().UsedBlocks(); got != 1 {
		t.Errorf("used blocks = %d, want 1", got)
	}
}

func TestEmptyFile(t *testing.T) {
	fs := newFS()
	if err := fs.WriteFile("/empty", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := fs.ReadFile("/empty")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("= %q, want empty", got)
	}
	// An empty file consumes no blocks.
	node, _ := fs.Stat("/empty")
	if len(node.Blocks) != 0 {
		t.Errorf("empty file holds %d blocks", len(node.Blocks))
	}
}

func TestDeepNesting(t *testing.T) {
	fs := New(64, 128)
	path := ""
	for range 20 {
		path += "/d"
		if err := fs.Mkdir(path, 0o755); err != nil {
			t.Fatalf("at depth %s: %v", path, err)
		}
	}
	if err := fs.WriteFile(path+"/deep.txt", []byte("found"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := fs.ReadFile(path + "/deep.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "found" {
		t.Errorf("= %q", got)
	}
}

func TestLargeFileThroughDescriptor(t *testing.T) {
	fs := New(32, 256)
	fd, err := fs.Open("/big.bin", O_CREATE|O_RDWR)
	if err != nil {
		t.Fatal(err)
	}

	// Write in many small pieces, crossing block boundaries repeatedly.
	var want bytes.Buffer
	for i := range 500 {
		chunk := []byte(strings.Repeat(string(rune('a'+i%26)), 7))
		if _, err := fs.Write(fd, chunk); err != nil {
			t.Fatal(err)
		}
		want.Write(chunk)
	}

	fs.Seek(fd, 0, io.SeekStart)
	got := make([]byte, want.Len())
	total := 0
	for total < len(got) {
		n, err := fs.Read(fd, got[total:])
		if err != nil && !errors.Is(err, io.EOF) {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
		total += n
	}
	fs.Close(fd)

	if !bytes.Equal(got[:total], want.Bytes()) {
		t.Errorf("large file round trip corrupted the data (%d of %d bytes match)", total, want.Len())
	}
}
