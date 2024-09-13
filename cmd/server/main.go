package main

import (
	"flag"
	"log"
	"os"
	"syscall"
	"tapfs"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

func main() {
	debug := flag.Bool("debug", false, "debug")
	origDir := flag.String("backing", "", "backing dir")
	depDir := flag.String("depdir", "", "dep dir")
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
	root := tapfs.NewTapFS(*origDir)
	sec := time.Second
	if *debug {
		sec = 0
	}
	server, err := fs.Mount(mntDir, root, &fs.Options{
		MountOptions:    fuse.MountOptions{Debug: *debug},
		UID:             uint32(os.Getuid()),
		GID:             uint32(os.Getgid()),
		EntryTimeout:    &sec,
		AttrTimeout:     &sec,
		NegativeTimeout: &sec,
	})
	if err != nil {
		log.Fatalf("Mount fail: %v\n", err)
	}
	log.Println("mounted!")

	if err := tapfs.StartCommandServer(root, *depDir, server, *debug); err != nil {
		log.Fatal(err)
	}

	server.WaitMount()
	// trigger ENOSYS
	syscall.Access(mntDir, 07)
	server.Wait()
}
