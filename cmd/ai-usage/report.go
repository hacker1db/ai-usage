package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"ai-usage/internal/report"
	"ai-usage/internal/usage"
)

func runReport(args []string) error {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	days := fs.Int("days", 30, "window in days")
	out := fs.String("out", "reports", "output root; a dated subfolder is created")
	print := fs.Bool("print", false, "also print the markdown report")
	opts := sourceFlags(fs)
	_ = fs.Parse(args)
	if *days < 1 {
		return fmt.Errorf("-days must be at least 1")
	}

	doc, agg := report.Build(time.Now(), *days, opts())
	dir, md, err := report.Write(doc, agg, *out)
	if err != nil {
		return err
	}
	if *print {
		fmt.Println(md)
	}
	for _, s := range doc.Sources {
		fmt.Fprintf(os.Stderr, "%-32s %-10s %8d requests  %s\n", s.Source, s.Status, s.Requests, s.Path)
	}
	fmt.Fprintf(os.Stderr, "wrote %s/{report.md,usage.json,usage.csv}\n", usage.Tilde(dir))
	return nil
}
