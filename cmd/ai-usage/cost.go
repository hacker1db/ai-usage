package main

import (
	"ai-usage/internal/billing"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

func runCost(args []string) error {
	fs := flag.NewFlagSet("cost", flag.ContinueOnError)
	provider := fs.String("provider", "", "azure or copilot")
	month := fs.String("month", time.Now().UTC().Format("2006-01"), "billing month YYYY-MM (UTC)")
	subscription := fs.String("subscription", "", "Azure subscription UUID")
	resources := fs.String("resources", "", "comma-separated full Foundry resource IDs")
	kind := fs.String("github-scope", "users", "users or organizations")
	account := fs.String("github-account", "", "explicit GitHub billing account")
	mode := fs.String("github-mode", "ai_credit", "ai_credit or premium_request (never summed)")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var r billing.Report
	var err error
	switch *provider {
	case "azure":
		var ids []string
		for _, id := range strings.Split(*resources, ",") {
			if id = strings.TrimSpace(id); id != "" {
				ids = append(ids, id)
			}
		}
		r, err = billing.Azure(ctx, billing.Command, *subscription, ids, *month)
	case "copilot":
		r, err = billing.Copilot(ctx, billing.Command, *kind, *account, *mode, *month)
	default:
		return fmt.Errorf("-provider must be azure or copilot")
	}
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}
