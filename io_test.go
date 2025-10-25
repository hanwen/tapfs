package tapfs

import (
	"crypto/sha256"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/hanwen/go-fuse/v2/fs"
)

func sha256hex(s string) Digest {
	h := sha256.New()
	io.WriteString(h, s)
	return Digest{fmt.Sprintf("%x", h.Sum(nil)), uint64(len(s))}
}

func TestBasic(t *testing.T) {
	orig := t.TempDir()

	os.WriteFile(orig+"/file1", []byte("x"), 0644)
	os.WriteFile(orig+"/file2", []byte("y"), 0644)
	os.WriteFile(orig+"/file3", []byte("z"), 0644)

	mnt := t.TempDir()
	db := t.TempDir()

	root, err := fs.NewLoopbackRoot(orig)
	debug := false

	server, err := NewCommandServer(root, mnt, db, debug)
	if err != nil {
		log.Fatal(err)
	}
	defer server.FSServer.Unmount()

	got, err := ClientRun(server.Addr(),
		"echo x >> file1 ; rm file2; sha1sum file3; echo y > file4", nil, mnt)

	if err != nil {
		t.Fatal(err)
	}

	if debug {
		c, err := os.ReadFile(filepath.Join(got.DepDir, got.ID+".log"))
		if err != nil {
			t.Error(err)
		}
		log.Println(string(c))
	}
	want := &TraceResponse{
		Operations: map[string]Operation{
			"file1": OpUpdate,
			"file2": OpDelete,
			"file3": OpRead,
			"file4": OpCreate,
		},
		Hashes: map[string]Digest{
			"file1": sha256hex("xx\n"),
			"file3": sha256hex("z"),
			"file4": sha256hex("y\n"),
		},
	}
	got.ID = ""
	got.DepDir = ""
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("-want, +got: %s", diff)
	}
}

func TestCache(t *testing.T) {
	orig := t.TempDir()
	mnt := t.TempDir()
	db := t.TempDir()

	os.WriteFile(orig+"/file1", []byte("x"), 0644)

	root, err := fs.NewLoopbackRoot(orig)
	server, err := NewCommandServer(root, mnt, db, false)
	if err != nil {
		log.Fatal(err)
	}
	defer server.FSServer.Unmount()

	cmd := `ninja_inputs='file1 '; ninja_outputs='file2 '; echo hello; cp file1 file2`
	got, err := ClientRun(server.Addr(), cmd, nil, mnt)
	if err != nil {
		t.Fatal(err)
	}

	want := &TraceResponse{
		Operations: map[string]Operation{
			"file1": OpRead,
			"file2": OpCreate,
		},
		Hashes: map[string]Digest{
			"file1": sha256hex("x"),
			"file2": sha256hex("x"),
		},
	}
	got.ID = ""
	got.DepDir = ""
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("-want, +got: %s", diff)
	}
	os.Remove(mnt + "/file2")
	got, err = ClientRun(server.Addr(), cmd, nil, mnt)
	if err != nil {
		t.Fatal(err)
	}
	got.ID = ""
	got.DepDir = ""
	want = &TraceResponse{
		CacheHit: &CacheHit{
			Stdout: []byte("hello\n"),
		},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("-want, +got: %s", diff)
	}
}
