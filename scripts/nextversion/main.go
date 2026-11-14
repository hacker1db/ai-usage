// Command nextversion prints the release plan for a commit as key=value lines, ready to append to
// $GITHUB_OUTPUT. It is used by .github/workflows/release.yml; see README.md, "Releases".
//
//	go run ./scripts/nextversion -rev HEAD -bump auto
package main

import (
	"flag"
	"fmt"
	"os"

	"ai-usage/internal/release"
)

func main() {
	rev := flag.String("rev", "HEAD", "commit to plan a release for")
	bump := flag.String("bump", "auto", "auto, or patch, minor, major to override the computed bump")
	dir := flag.String("C", ".", "repository directory")
	flag.Parse()

	force := release.None
	if *bump != "auto" {
		b, err := release.ParseBump(*bump)
		if err != nil {
			fail(err)
		}
		force = b
	}
	p, err := release.MakePlan(release.Git{Dir: *dir}, *rev, force)
	if err != nil {
		fail(err)
	}
	fmt.Printf("action=%s\ntag=%s\nprevious=%s\ncommit=%s\nreason=%s\n", p.Action, p.Tag, p.Previous, p.Commit, p.Reason)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "nextversion:", err)
	os.Exit(1)
}
