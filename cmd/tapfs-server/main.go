package main

import (
	"crypto/sha256"
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"tapfs"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/hanwen/go-fuse/v2/fs"

	"github.com/hanwen/grit/gritfs"
	"github.com/hanwen/grit/repo"
)

func main() {
	if err := mainErr(); err != nil {
		log.Fatal(err)
	}
}

func mainErr() error {
	debug := flag.Bool("debug", false, "debug")
	origDir := flag.String("loopback", "", "use loopback with this backing dir")
	repoPath := flag.String("repo", "", "use git repository with this backing dir")
	originURL := flag.String("repo_url", "", "URL to remote git repo")
	commitSHA := flag.String("repo_commit", "", "sha1 commit to check out")
	dbDir := flag.String("database", "", "database dir")
	flag.Parse()
	if flag.NArg() == 0 {
		return fmt.Errorf("must specify mount dir")
	}
	mntDir := flag.Arg(0)
	if *dbDir == "" {
		return fmt.Errorf("must set --database")
	}
	acDir := filepath.Join(*dbDir, "ac")
	casDir := filepath.Join(*dbDir, "cas")
	os.MkdirAll(acDir, 0755)
	os.MkdirAll(casDir, 0755)
	var cas tapfs.CAS
	ac := tapfs.NewDiskActionCache(acDir)

	var err error
	var root fs.InodeEmbedder
	if *origDir != "" {
		root, err = fs.NewLoopbackRoot(*origDir)
		if err != nil {
			return err
		}
		cas = tapfs.NewDiskCAS(casDir, sha256.New, false)
	} else {
		if fi, err := os.Stat(filepath.Join(*repoPath, ".git")); err == nil && fi.IsDir() {
			*repoPath = filepath.Join(*repoPath, ".git")
		}

		gitRepo, err := git.PlainOpen(*repoPath)
		if err != nil {
			return fmt.Errorf("PlainOpen(%q): %v", *repoPath, err)
		}

		gitCasDir := filepath.Join(*dbDir, "gitcas")
		os.MkdirAll(gitCasDir, 0755)
		gritCas, err := gritfs.NewCAS(gitCasDir)
		if err != nil {
			return fmt.Errorf("NewCAS: %v", err)
		}

		// submodule URLs are relative; resolution goes wrong if it
		// doesn't end in '/'.
		if !strings.HasSuffix(*originURL, "/") {
			*originURL += "/"
		}
		repoURL, err := url.Parse(*originURL)
		if err != nil {
			return fmt.Errorf("Parse(%q): %v", *originURL, err)
		}

		gritRepo, err := repo.NewRepo(gitRepo, *repoPath, repoURL)
		if err != nil {
			return fmt.Errorf("NewRepo(%q, %s): %v", *repoPath, repoURL, err)
		}

		rootNode, err := gritfs.NewRoot(gritCas, gritRepo, "workspace")
		if err != nil {
			return fmt.Errorf("NewRoot: %v", err)
		}

		root = rootNode
		cas = tapfs.NewGritCASAdapter(gritCas)
	}
	if root == nil {
		return fmt.Errorf("must set --backing or --repo,--repo_url")
	}
	server, err := tapfs.NewCommandServer(root, mntDir, cas, ac, *debug)
	if err != nil {
		return fmt.Errorf("NewCommandServer: %v", err)
	}
	defer server.Close()

	if *commitSHA != "" {
		re := regexp.MustCompile("[0-9a-f]{40}")
		if !re.MatchString(*commitSHA) {
			return fmt.Errorf("not a sha1: %q", *commitSHA)
		}
		id := plumbing.NewHash(*commitSHA)
		if err := root.(*gritfs.RepoNode).SetID(id, time.Now()); err != nil {
			return fmt.Errorf("SetID: %v", err)
		}
	}

	log.Printf("tapfs ready")
	server.Wait()
	return nil
}
