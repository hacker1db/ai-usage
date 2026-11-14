package sources

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"ai-usage/internal/usage"
)

// foundryHostRE matches an Azure AI Foundry / OpenAI endpoint host and captures its subdomain.
var foundryHostRE = regexp.MustCompile(`(?i)https://([a-z0-9][a-z0-9-]{1,62})\.(?:services\.ai\.azure\.com|openai\.azure\.com|cognitiveservices\.azure\.com)`)

var subdomainRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}$`)

// FoundryEndpoints returns the Azure Foundry endpoint subdomains the local tools are configured to
// use: ANTHROPIC_FOUNDRY_RESOURCE / ANTHROPIC_FOUNDRY_BASE_URL in each Claude profile's
// settings.json and the environment, and Azure base_url hosts in Codex's config.toml. Billing uses
// them to find the matching Azure resources, so nothing about the account is hardcoded.
func FoundryEndpoints(o Options) []string {
	seen := map[string]bool{}
	add := func(resource, baseURL string) {
		if r := strings.ToLower(strings.TrimSpace(resource)); subdomainRE.MatchString(r) {
			seen[r] = true
		}
		if m := foundryHostRE.FindStringSubmatch(baseURL); m != nil {
			seen[strings.ToLower(m[1])] = true
		} else if u, err := url.Parse(strings.TrimSpace(baseURL)); err == nil && u.Host != "" {
			if m := foundryHostRE.FindStringSubmatch("https://" + u.Host); m != nil {
				seen[strings.ToLower(m[1])] = true
			}
		}
	}
	add(os.Getenv("ANTHROPIC_FOUNDRY_RESOURCE"), os.Getenv("ANTHROPIC_FOUNDRY_BASE_URL"))
	dirs := o.Claude
	if dirs == nil {
		dirs = DefaultClaudeDirs()
	}
	for _, d := range dirs {
		raw, err := os.ReadFile(filepath.Join(usage.ExpandHome(d.Path), "settings.json"))
		if err != nil {
			continue
		}
		var s struct{ Env map[string]string }
		if json.Unmarshal(raw, &s) == nil {
			add(s.Env["ANTHROPIC_FOUNDRY_RESOURCE"], s.Env["ANTHROPIC_FOUNDRY_BASE_URL"])
		}
	}
	for _, home := range o.CodexHomes() {
		if raw, err := os.ReadFile(filepath.Join(usage.ExpandHome(home), "config.toml")); err == nil {
			for _, m := range foundryHostRE.FindAllStringSubmatch(string(raw), -1) {
				seen[strings.ToLower(m[1])] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
