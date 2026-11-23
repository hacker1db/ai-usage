package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"path"
	"strconv"
	"strings"
)

func main() {
	profile := flag.String("profile", "coverage.out", "coverage profile from go test -coverprofile")
	pkg := flag.String("pkg", "", "import path of the package to check (required)")
	floor := flag.Float64("min", 0, "minimum statement coverage percent")
	flag.Parse()
	if *pkg == "" {
		fmt.Fprintln(os.Stderr, "covfloor: -pkg is required")
		os.Exit(2)
	}
	pct, err := coverage(*profile, *pkg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "covfloor:", err)
		os.Exit(2)
	}
	fmt.Printf("%s coverage: %.1f%% (floor %.1f%%)\n", *pkg, pct, *floor)
	if pct < *floor {
		fmt.Fprintln(os.Stderr, "covfloor: coverage below floor")
		os.Exit(1)
	}
}

func coverage(profile, pkg string) (float64, error) {
	f, err := os.Open(profile)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	type block struct{ stmts, hit int }
	blocks := map[string]block{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "mode:") || line == "" {
			continue
		}
		loc, rest, ok := strings.Cut(line, " ")
		file, _, _ := strings.Cut(loc, ":")
		if !ok || path.Dir(file) != pkg {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) != 2 {
			return 0, fmt.Errorf("malformed profile line %q", line)
		}
		stmts, err1 := strconv.Atoi(fields[0])
		count, err2 := strconv.Atoi(fields[1])
		if err1 != nil || err2 != nil {
			return 0, fmt.Errorf("malformed profile line %q", line)
		}
		b := blocks[loc]
		b.stmts = stmts
		if count > 0 {
			b.hit = stmts
		}
		blocks[loc] = b
	}
	if err := sc.Err(); err != nil {
		return 0, err
	}
	var total, hit int
	for _, b := range blocks {
		total += b.stmts
		hit += b.hit
	}
	if total == 0 {
		return 0, fmt.Errorf("no statements for %s in %s", pkg, profile)
	}
	return 100 * float64(hit) / float64(total), nil
}
