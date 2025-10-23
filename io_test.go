package tapfs

import (
	"crypto/sha256"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/hanwen/go-fuse/v2/fs"
)

func sha256hex(s string) string {
	h := sha256.New()
	io.WriteString(h, s)
	return fmt.Sprintf("%x", h.Sum(nil))
}

func TestBasic(t *testing.T) {
	orig := t.TempDir()

	os.WriteFile(orig+"/file1", []byte("x"), 0644)
	os.WriteFile(orig+"/file2", []byte("y"), 0644)
	os.WriteFile(orig+"/file3", []byte("z"), 0644)

	mnt := t.TempDir()
	db := t.TempDir()

	root, err := fs.NewLoopbackRoot(orig)
	debug := true

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
		Update: []string{"file1"},
		Read:   []string{"file3"},
		Create: []string{"file4"},
		Delete: []string{"file2"},
		Hashes: map[string]string{
			"file1": sha256hex("xx\n"),
			"file2": strings.Repeat("00", sha256.Size),
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
