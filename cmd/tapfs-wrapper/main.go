package main

import (
	"flag"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"tapfs"
)

func main() {
	dir := flag.String("dir", "", "tapfs checkout; defaults to $CWD")
	c := flag.String("c", "", "command")
	flag.Parse()

	if *dir == "" {
		var err error
		*dir, err = os.Getwd()
		if err != nil {
			log.Fatal(err)
		}
	}

	for _, c := range strings.Split(os.Getenv("PATH"), ":") {
		if strings.Contains(c, "ccache") {
			log.Fatalf("found 'ccache' in $PATH, %q", c)
		}
	}

	if *c == "" {
		log.Fatalf("must set -c")
	}
	*dir = filepath.Clean(*dir)
	sock, _, err := tapfs.FindSocket(*dir)
	if err != nil {
		log.Fatal(err)
	}

	wd, err := os.Getwd()
	if err != nil {
		log.Fatal("Getwd", err)
	}
	rep, err := tapfs.ClientRun(sock, *c, os.Environ(), wd, true)
	if err != nil {
		log.Fatalf("ClientRun: %v", err)
	}
	if rep.CacheHit != nil {
		os.Stdout.Write(rep.CacheHit.Stdout)
		os.Stderr.Write(rep.CacheHit.Stderr)
	}
	if ex, ok := err.(*exec.ExitError); ok {
		os.Exit(ex.ExitCode())
	}
	if err != nil {
		log.Fatal(err)
	}
	os.Exit(0)
}
