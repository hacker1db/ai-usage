package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"ai-usage/internal/plancost"
	"ai-usage/internal/sources"
	"ai-usage/internal/usage"
)

type sessionList []string

func (s *sessionList) String() string { return strings.Join(*s, ",") }

func (s *sessionList) Set(v string) error {
	for _, ref := range strings.Split(v, ",") {
		if ref = strings.TrimSpace(ref); ref != "" {
			*s = append(*s, ref)
		}
	}
	return nil
}

const exitIncomplete = 3

func runPlanCost(args []string) error {
	fs := flag.NewFlagSet("plan-cost", flag.ContinueOnError)
	var refs sessionList
	fs.Var(&refs, "session", "session `tool|profile|id` or bare id (repeatable, or comma-separated)")
	pricingPath := fs.String("pricing", plancost.DefaultPricingPath, "pricing file (USD per million tokens)")
	asJSON := fs.Bool("json", false, "print the summary as JSON")
	strict := fs.Bool("strict", false, "exit 3 when the summary is incomplete")
	opts := sourceFlags(fs)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments %q; pass sessions with -session", fs.Args())
	}
	if len(refs) == 0 {
		return fmt.Errorf("at least one -session is required")
	}
	pricing, err := plancost.LoadPricing(*pricingPath)
	if err != nil {
		return err
	}

	agg := usage.NewAggregator(time.Time{}, usage.Daily)
	status, _ := sources.CollectAll(agg, opts(), nil)
	var sourceErrors []string
	for _, st := range status {
		if st.Status != "ok" && st.Status != "not found" {
			sourceErrors = append(sourceErrors, st.Source+": "+st.Status)
		}
		for _, e := range st.Errors {
			sourceErrors = append(sourceErrors, st.Source+": "+e)
		}
	}
	sum, err := plancost.Compute(agg, refs, pricing, sourceErrors, time.Now())
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(sum); err != nil {
			return err
		}
	} else {
		printPlanCost(sum)
	}
	if *strict && !sum.Complete {
		os.Exit(exitIncomplete)
	}
	return nil
}

func printPlanCost(s plancost.Summary) {
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "SESSION\tREQUESTS\tTOTAL\tUNCACHED\tOUTPUT\tCOST (USD)")
	for _, sc := range s.Sessions {
		fmt.Fprintf(w, "%s\t%d\t%d\t%d\t%d\t%s\n", sc.Key, sc.Totals.Requests, sc.Totals.Total,
			sc.Totals.Uncached(), sc.Totals.Output, money(sc.CostUSD, sc.PricedCostUSD))
	}
	fmt.Fprintf(w, "TOTAL\t%d\t%d\t%d\t%d\t%s\n", s.Totals.Requests, s.Totals.Total, s.Totals.Uncached(),
		s.Totals.Output, money(s.EstimatedCostUSD, s.PricedCostUSD))
	_ = w.Flush()
	if !s.Pricing.Loaded {
		fmt.Printf("\npricing file %s not found: costs are unknown\n", s.Pricing.Path)
	}
	for _, label := range []struct {
		name string
		v    []string
	}{{"missing sessions", s.MissingSessions}, {"unpriced models", s.UnpricedModels}, {"source errors", s.SourceErrors}} {
		if len(label.v) > 0 {
			fmt.Printf("%s: %s\n", label.name, strings.Join(label.v, ", "))
		}
	}
	if !s.Complete {
		fmt.Println("INCOMPLETE: totals above are a lower bound")
	}
}

func money(full *float64, priced float64) string {
	if full != nil {
		return fmt.Sprintf("%.4f", *full)
	}
	return fmt.Sprintf(">= %.4f", priced)
}
