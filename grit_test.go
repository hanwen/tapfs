package tapfs

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/hanwen/grit/gitutil"
	"github.com/hanwen/grit/gritfs"
	"github.com/hanwen/grit/repo"
)

func githash(s string) string {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\000%s", len(s), s)
	return hex.EncodeToString(h.Sum(nil))
}

func TestGritHash(t *testing.T) {
	input := map[string]string{
		"a":     "hello world",
		"b/c":   "xyz",
		"b/d":   "pqr",
		"large": strings.Repeat("x", 20000),
	}
	srcRoot := t.TempDir()

	tr, err := gitutil.SetupTestRepo(srcRoot, "repo", input)
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.Serve(srcRoot); err != nil {
		t.Fatal(err)
	}

	casDir := t.TempDir()
	cas, err := gritfs.NewCAS(casDir)
	if err != nil {
		t.Fatal(err)
	}

	gitRepo, err := git.PlainOpen(filepath.Join(srcRoot, "repo"))
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(tr.RepoURL)
	if err != nil {
		t.Fatal(err)
	}
	gritRepo, err := repo.NewRepo(gitRepo, filepath.Join(srcRoot, "repo"), u)
	if err != nil {
		t.Fatal(err)
	}

	repoNode, err := gritfs.NewRoot(cas, gritRepo, "ws")
	if err != nil {
		t.Fatalf("NewRoot: %v", err)
	}
	acDir := t.TempDir()
	ac := NewDiskActionCache(acDir)

	mntDir := t.TempDir()
	debug := false

	adapter := NewGritCASAdapter(cas)
	server, err := NewCommandServer(repoNode, mntDir, adapter, ac, debug)
	if err != nil {
		t.Fatalf("NewCommandServer: %v", err)
	}
	defer server.Close()
	defer server.FSServer.Unmount()

	if err := repoNode.SetID(tr.CommitID, time.Now()); err != nil {
		t.Fatalf("SetID: %v", err)
	}
	log.Printf("child %s, parent %p, %v", repoNode.GetChild("a"), repoNode.EmbeddedInode(), repoNode.Children())
	if err := os.WriteFile(mntDir+"/file.txt", []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	cmd := `wc a ; echo x > t`
	rep, err := ClientRun(server.Addr(), cmd, nil, mntDir, true)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]Operation{"a": OpRead, "t": OpCreate}; !reflect.DeepEqual(want, rep.Operations) {
		t.Errorf("got %v want %v", rep.Operations, want)
	}

	output := "x\n"
	if want := map[string]FileInfo{"a": FileInfo{Digest: Digest{
		Hash: githash("hello world"),
		Size: 11,
	}}, "t": FileInfo{Digest: Digest{Hash: githash(output), Size: 2}}}; !reflect.DeepEqual(want, rep.Files) {
		t.Errorf("got %v want %v", rep.Files, want)
	}

	if _, err := repoNode.Snapshot(&gritfs.WorkspaceUpdate{TS: time.Now()}); err != nil {
		t.Fatal(err)
	}

	if err := repoNode.SetID(tr.CommitID, time.Now()); err != nil {
		t.Fatalf("SetID: %v", err)
	}

	repoNode.NotifyEntry("t")
	if fi, err := os.Lstat(mntDir + "/t"); err == nil {
		t.Fatalf("Lstat after SetID: %v", fi)
	}

	rep, err = ClientRun(server.Addr(), cmd, nil, mntDir, true)
	if err != nil {
		t.Fatal(err)
	}
	if rep.CacheHit == nil {
		t.Errorf("no cache hit")
	}
	ch, left := nodeAt(repoNode.EmbeddedInode(), "t")
	if left != "" {
		t.Errorf("no path to file: %q", left)
	}

	if fi, err := os.Lstat(mntDir + "/t"); err != nil {
		t.Fatalf("Lstat after cache hit: %v", err)
	} else if fi.Size() != 2 {
		t.Errorf("size %d, want 2", fi.Size())
	}
	bn, ok := ch.Operations().(*blobTapFSNode)
	if !ok {
		t.Errorf("not a *blobTapFSNode, but %T", ch.Operations())
	}

	if got, want := bn.BlobNode.ID().String(), githash("x\n"); got != want {
		t.Errorf("got %s, want %s", got, want)
	}

	c, err := os.ReadFile(mntDir + "/t")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(c) != output {
		t.Fatalf("got %q want %q", c, output)
	}

	// Ninja build rebuilds checked-in sources. It uses a `rm dest
	// ; rename tmp dest`, but falls back to `cp tmp dest; rm
	// tmp`.
	cmd = `rm b/d ; echo -e pqr > b/d`
	rep, err = ClientRun(server.Addr(), cmd, nil, mntDir, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := repoNode.SetID(tr.CommitID, time.Now()); err != nil {
		t.Fatalf("SetID: %v", err)
	}

	repoNode.GetChild("b").NotifyEntry("d")

	rep, err = ClientRun(server.Addr(), cmd, nil, mntDir, true)
	if err != nil {
		t.Fatal(err)
	}
	if rep.CacheHit == nil {
		t.Errorf("expected cache hit")
	}
}
