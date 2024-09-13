package main

import (
	"flag"
	"fmt"
	"log"
	"tapfs"
)

func main() {
	flag.Parse()

	if len(flag.Args()) != 1 {
		log.Fatal("must specify dir as arg")
	}
	data, err := tapfs.Readdir(flag.Arg(0))
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println("digraph {")
	for _, d := range data {
		if len(d.Create)+len(d.Update) == 0 {
			continue
		}
		for _, r := range d.Read {
			fmt.Printf("%q -> %q;\n", r, d.ID)
		}
		for _, w := range d.Create {
			fmt.Printf("%q -> %q;\n", d.ID, w)
		}
		for _, w := range d.Update {
			fmt.Printf("%q -> %q;\n", d.ID, w)
		}
		fmt.Println()
	}

	fmt.Println("}")
}
