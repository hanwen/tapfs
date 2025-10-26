package main

import (
	"crypto/sha256"
	"flag"
	"log"
	"os"
	"path/filepath"
	"tapfs"

	"github.com/hanwen/go-fuse/v2/fs"
)

func main() {
	debug := flag.Bool("debug", false, "debug")
	origDir := flag.String("backing", "", "backing dir")
	dbDir := flag.String("database", "", "database dir")
	flag.Parse()
	if flag.NArg() == 0 {
		log.Fatal("must specify mount dir")
	}
	mntDir := flag.Arg(0)
	if *origDir == "" {
		log.Fatal("must set --backing")
	}
	if *dbDir == "" {
		log.Fatal("must set --database")
	}

	acDir := filepath.Join(*dbDir, "ac")
	casDir := filepath.Join(*dbDir, "cas")
	os.MkdirAll(acDir, 0755)
	os.MkdirAll(casDir, 0755)
	cas := tapfs.NewDiskCAS(casDir, sha256.New)
	ac := tapfs.NewDiskActionCache(acDir)

	root, err := fs.NewLoopbackRoot(*origDir)
	if err != nil {
		log.Fatal(err)
	}

	server, err := tapfs.NewCommandServer(root, mntDir, cas, ac, *debug)
	if err != nil {
		log.Fatal(err)
	}
	defer server.Close()
	log.Printf("tapfs ready")
	server.Wait()
}
