package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"ai-usage/internal/fep/token"
	"ai-usage/internal/m365"
	"ai-usage/internal/paths"
	"ai-usage/internal/usage"
)

// runM365Sync fetches the signed-in user's Microsoft 365 Copilot daily usage from the Graph usage
// report and merges it into the local store the dashboard reads. Run it daily (the report covers
// only its latest eight days); history older than that is kept from earlier syncs.
func runM365Sync(args []string) error {
	fs := flag.NewFlagSet("m365-sync", flag.ContinueOnError)
	store := fs.String("store", usage.Tilde(paths.M365Store()), "store file to update")
	upn := fs.String("upn", "", "your user principal name (default: the Azure CLI's signed-in user)")
	tenant := fs.String("tenant", "", "Entra tenant id (default: the Azure CLI's current tenant)")
	az := fs.String("az", "az", "Azure CLI binary")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	azPath, err := exec.LookPath(*az)
	if err != nil {
		return fmt.Errorf("azure cli not found (%v); install it and run `az login`", err)
	}
	if *upn == "" {
		out, err := exec.CommandContext(ctx, azPath, "account", "show", "--query", "user.name", "-o", "tsv").Output()
		if err != nil {
			return fmt.Errorf("az account show: %v; run `az login` or pass -upn", err)
		}
		*upn = strings.TrimSpace(string(out))
		if *upn == "" {
			return errors.New("az account show returned no user; pass -upn")
		}
	}
	c := &m365.Client{Tokens: token.NewAzCLI(azPath, m365.Scope, *tenant)}
	days, err := c.Sync(ctx, *upn, time.Now())
	if err != nil && len(days) == 0 {
		return err
	}
	path := usage.ExpandHome(*store)
	s, lerr := m365.Load(path)
	if lerr != nil {
		return lerr
	}
	s.Merge(*upn, days)
	if serr := s.Save(path); serr != nil {
		return serr
	}
	var actions int64
	for _, d := range days {
		actions += d.Actions
	}
	fmt.Fprintf(os.Stderr, "m365-sync: %d days, %d Copilot actions for %s -> %s (%d days stored)\n",
		len(days), actions, *upn, usage.Tilde(path), len(s.Days))
	return err // a partial sync is saved but still reported
}
