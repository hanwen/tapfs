package tapfs

import (
	"crypto/sha256"
	"log"
	"os"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/hanwen/go-fuse/v2/fs"
)

func TestBasic(t *testing.T) {
	orig := t.TempDir()

	os.WriteFile(orig+"/file1", []byte("x"), 0644)
	os.WriteFile(orig+"/file2", []byte("y"), 0644)
	os.WriteFile(orig+"/file3", []byte("z"), 0644)

	mnt := t.TempDir()

	root, err := fs.NewLoopbackRoot(orig)
	debug := false

	cas := NewMemCAS(sha256.New, false)
	ac := NewMemActionCache()
	server, err := NewCommandServer(root, mnt, cas, ac, debug)
	if err != nil {
		log.Fatal(err)
	}
	defer server.FSServer.Unmount()

	got, err := ClientRun(server.Addr(),
		"echo x >> file1 ; rm file2; sha1sum file3; echo y > file4; echo '#!' > exe ; chmod +x exe", nil, mnt, false)

	if err != nil {
		t.Fatal(err)
	}

	want := &TraceResponse{
		Operations: map[string]Operation{
			"file1": OpUpdate,
			"file2": OpDelete,
			"file3": OpRead,
			"file4": OpCreate,
			"exe":   OpCreate,
		},
		Files: map[string]FileInfo{
			"file1": FileInfo{Digest: sha256hex("xx\n")},
			"file3": FileInfo{Digest: sha256hex("z")},
			"file4": FileInfo{Digest: sha256hex("y\n")},
			"exe":   FileInfo{Digest: sha256hex("#!\n"), Type: FileExecutable},
		},
	}
	got.ID = ""
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("-want, +got: %s", diff)
	}
}

func TestCacheBasic(t *testing.T) {
	orig := t.TempDir()
	mnt := t.TempDir()

	os.WriteFile(orig+"/file1", []byte("x"), 0644)

	root, err := fs.NewLoopbackRoot(orig)
	cas := NewMemCAS(sha256.New, false)
	ac := NewMemActionCache()
	server, err := NewCommandServer(root, mnt, cas, ac, false)
	if err != nil {
		log.Fatal(err)
	}
	defer server.FSServer.Unmount()

	cmd := `ninja_inputs='file1 '; ninja_outputs='file2 '; echo hello; cp file1 file2; cp file1 exe ; chmod 755 exe`
	got, err := ClientRun(server.Addr(), cmd, nil, mnt, true)
	if err != nil {
		t.Fatal(err)
	}

	if len(ac.cache) != 1 {
		t.Fatal("action store failed")
	}

	want := &TraceResponse{
		Operations: map[string]Operation{
			"file1": OpRead,
			"file2": OpCreate,
			"exe":   OpCreate,
		},
		Files: map[string]FileInfo{
			"file1": FileInfo{Digest: sha256hex("x")},
			"file2": FileInfo{Digest: sha256hex("x")},
			"exe":   FileInfo{Digest: sha256hex("x"), Type: FileExecutable},
		},
	}
	got.ID = ""
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("-want, +got: %s", diff)
	}
	os.Remove(mnt + "/file2")
	os.Remove(mnt + "/exe")
	got, err = ClientRun(server.Addr(), cmd, nil, mnt, true)
	if err != nil {
		t.Fatal(err)
	}
	got.ID = ""
	want = &TraceResponse{
		CacheHit: &CacheHit{
			Stdout: []byte("hello\n"),
		},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("-want, +got: %s", diff)
	}

	fi, err := os.Lstat(mnt + "/exe")
	exe := fi.Mode()&0111 != 0

	if !exe {
		t.Errorf("want exe got mode %o", fi.Mode())
	}
}

func TestCacheDir(t *testing.T) {
	orig := t.TempDir()
	mnt := t.TempDir()

	os.WriteFile(orig+"/file1", []byte("x"), 0644)

	root, err := fs.NewLoopbackRoot(orig)
	cas := NewMemCAS(sha256.New, false)
	ac := NewMemActionCache()
	server, err := NewCommandServer(root, mnt, cas, ac, false)
	if err != nil {
		log.Fatal(err)
	}
	defer server.FSServer.Unmount()

	cmd := `mkdir build ; echo  x > build/file`
	if _, err := ClientRun(server.Addr(), cmd, nil, mnt, true); err != nil {
		t.Fatal(err)
	}

	os.Remove(mnt + "/build/file")
	os.Remove(mnt + "/build")
	// Mimick Ninja, which will rebuild the non-existent (failed stat) file.
	if _, err := os.Lstat(mnt + "/build"); err == nil {
		t.Fatal("stat should have failed")
	}

	_, err = ClientRun(server.Addr(), cmd, nil, mnt, true)
	if err != nil {
		t.Fatal(err)
	}
}

func TestCacheDeletion(t *testing.T) {
	orig := t.TempDir()
	mnt := t.TempDir()

	os.WriteFile(orig+"/file1", []byte("x"), 0644)

	root, err := fs.NewLoopbackRoot(orig)
	cas := NewMemCAS(sha256.New, false)
	ac := NewMemActionCache()
	server, err := NewCommandServer(root, mnt, cas, ac, false)
	if err != nil {
		log.Fatal(err)
	}
	defer server.FSServer.Unmount()

	cmd := `echo x  > t; cp t t2 ; rm t; mv t2 t3`
	if _, err := ClientRun(server.Addr(), cmd, nil, mnt, true); err != nil {
		t.Fatal(err)
	}
}

func TestFileHash(t *testing.T) {
	orig := t.TempDir()
	mnt := t.TempDir()

	os.WriteFile(orig+"/file1", []byte("x"), 0644)

	root, err := fs.NewLoopbackRoot(orig)
	cas := NewMemCAS(sha256.New, false)
	ac := NewMemActionCache()
	server, err := NewCommandServer(root, mnt, cas, ac, false)
	if err != nil {
		log.Fatal(err)
	}
	defer server.FSServer.Unmount()

	if err := os.WriteFile(mnt+"/file", []byte("xyz"), 0755); err != nil {
		t.Fatal(err)
	}

	got, err := server.root.(*loopbackTapFSNode).GetChild("file").Operations().(*loopbackTapFSNode).GetFileInfo(cas)
	if err != nil {
		t.Fatal(err)
	}

	want := FileInfo{
		Digest: sha256hex("xyz"),
		Type:   FileExecutable,
	}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("-want, +got: %s", diff)
	}
}
