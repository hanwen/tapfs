package main

import (
	"flag"
	"log"
	"tapfs"

	"github.com/hanwen/go-fuse/v2/fs"
)

func main() {
	debug := flag.Bool("debug", false, "debug")
	origDir := flag.String("backing", "", "backing dir")
	depDir := flag.String("database", "", "database dir")
	flag.Parse()
	if flag.NArg() == 0 {
		log.Fatal("must specify mount dir")
	}
	mntDir := flag.Arg(0)
	if *origDir == "" {
		log.Fatal("must set --backing")
	}
	if *depDir == "" {
		log.Fatal("must set --depdir")
	}
	root, err := fs.NewLoopbackRoot(*origDir)
	if err != nil {
		log.Fatal(err)
	}

	server, err := tapfs.NewCommandServer(root, mntDir, *depDir, *debug)
	if err != nil {
		log.Fatal(err)
	}
	defer server.Close()
	log.Printf("tapfs ready")
	server.Wait()
}
