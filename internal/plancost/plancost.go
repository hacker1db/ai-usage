// Package plancost totals the token usage and estimated cost of an explicit set of sessions, so a
// plan (or any other unit of work) can record what it cost. Sessions are linked by hand; nothing
// here guesses which sessions belong together.
package plancost

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"sort"
	"strings"
	"time"

	"ai-usage/internal/usage"
)

// Version is the schema version of Summary. Bump it on any incompatible JSON change; plan-manager
// refuses versions it does not know.
const Version = 1

// DefaultPricingPath is where Load looks when no -pricing flag is given.
const DefaultPricingPath = "~/.config/ai-usage/pricing.json"

// Price is USD per million tokens. Every field is required: a typo or a missing rate must fail
// loudly instead of pricing those tokens at zero.
type Price struct {
	Input       float64 `json:"input"`        // input that was neither a cache read nor a cache write
	CachedInput float64 `json:"cached_input"` // cache reads
	CacheWrite  float64 `json:"cache_write"`  // cache writes (creation)
	Output      float64 `json:"output"`       // output, including reasoning
}

// Pricing maps model names to prices. A key ending in "*" matches any model with that prefix;
// an exact key wins over a prefix, and the longest prefix wins among prefixes.
type Pricing struct {
	Path   string // as given, for display
	SHA256 string // of the file content, so a saved summary says which prices produced it
	Loaded bool   // false when the file does not exist
	exact  map[string]Price
	prefix []prefixPrice // longest first
}

type prefixPrice struct {
	prefix string
	price  Price
}

type pricingFile struct {
	Currency string                     `json:"currency"`
	Unit     string                     `json:"unit"`
	Models   map[string]json.RawMessage `json:"models"`
}

// LoadPricing reads a pricing file. A missing file is not an error: every model is then unpriced
// and the summary is marked incomplete. Anything malformed is an error.
func LoadPricing(path string) (*Pricing, error) {
	p := &Pricing{Path: path, exact: map[string]Price{}}
	data, err := os.ReadFile(usage.ExpandHome(path))
	if errors.Is(err, fs.ErrNotExist) {
		return p, nil
	}
	if err != nil {
		return nil, fmt.Errorf("pricing: %w", err)
	}
	sum := sha256.Sum256(data)
	p.SHA256 = hex.EncodeToString(sum[:])
	var f pricingFile
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("pricing %s: %w", path, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("pricing %s: trailing data after the JSON object", path)
	}
	if f.Currency != "USD" {
		return nil, fmt.Errorf("pricing %s: currency must be \"USD\", got %q", path, f.Currency)
	}
	if f.Unit != "per_million_tokens" {
		return nil, fmt.Errorf("pricing %s: unit must be \"per_million_tokens\", got %q", path, f.Unit)
	}
	for name, raw := range f.Models {
		price, err := decodePrice(raw)
		if err != nil {
			return nil, fmt.Errorf("pricing %s: model %q: %w", path, name, err)
		}
		switch {
		case name == "" || name == "*":
			return nil, fmt.Errorf("pricing %s: model key %q matches everything; name the models", path, name)
		case strings.Count(name, "*") > 1 || (strings.Contains(name, "*") && !strings.HasSuffix(name, "*")):
			return nil, fmt.Errorf("pricing %s: model key %q: \"*\" is only allowed once, at the end", path, name)
		case strings.HasSuffix(name, "*"):
			p.prefix = append(p.prefix, prefixPrice{strings.TrimSuffix(name, "*"), price})
		default:
			p.exact[name] = price
		}
	}
	sort.Slice(p.prefix, func(i, j int) bool {
		if len(p.prefix[i].prefix) != len(p.prefix[j].prefix) {
			return len(p.prefix[i].prefix) > len(p.prefix[j].prefix)
		}
		return p.prefix[i].prefix < p.prefix[j].prefix
	})
	p.Loaded = true
	return p, nil
}

func decodePrice(raw json.RawMessage) (Price, error) {
	var fields map[string]*float64
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&fields); err != nil {
		return Price{}, err
	}
	var p Price
	for _, f := range []struct {
		name string
		dst  *float64
	}{{"input", &p.Input}, {"cached_input", &p.CachedInput}, {"cache_write", &p.CacheWrite}, {"output", &p.Output}} {
		v, ok := fields[f.name]
		if !ok || v == nil {
			return Price{}, fmt.Errorf("missing %q", f.name)
		}
		if *v < 0 || math.IsNaN(*v) || math.IsInf(*v, 0) {
			return Price{}, fmt.Errorf("%q must be a finite number >= 0", f.name)
		}
		*f.dst = *v
		delete(fields, f.name)
	}
	for k := range fields {
		return Price{}, fmt.Errorf("unknown field %q", k)
	}
	return p, nil
}

// Lookup returns the price for a model.
func (p *Pricing) Lookup(model string) (Price, bool) {
	if p == nil {
		return Price{}, false
	}
	if v, ok := p.exact[model]; ok {
		return v, true
	}
	for _, pp := range p.prefix {
		if strings.HasPrefix(model, pp.prefix) {
			return pp.price, true
		}
	}
	return Price{}, false
}

// Cost prices m. Input includes cache reads and writes, so fresh input is what is left after both.
func (p Price) Cost(m usage.Metrics) float64 {
	fresh := max(m.Input-m.Cached-m.CacheWrite, 0)
	return (float64(fresh)*p.Input + float64(m.Cached)*p.CachedInput +
		float64(m.CacheWrite)*p.CacheWrite + float64(m.Output)*p.Output) / 1e6
}

// ModelCost is one model's usage and cost. CostUSD is null when the model has no price.
type ModelCost struct {
	Model string `json:"model"`
	usage.Metrics
	CostUSD *float64 `json:"cost_usd"`
}

// SessionCost is one linked session.
type SessionCost struct {
	Key     string        `json:"key"` // tool|profile|id
	Tool    string        `json:"tool"`
	Profile string        `json:"profile"`
	ID      string        `json:"id"`
	Title   string        `json:"title"`
	First   string        `json:"first"`
	Last    string        `json:"last"`
	Totals  usage.Metrics `json:"totals"`
	// CostUSD is null unless every model in the session is priced; PricedCostUSD is the priced part.
	CostUSD       *float64    `json:"cost_usd"`
	PricedCostUSD float64     `json:"priced_cost_usd"`
	Models        []ModelCost `json:"models"`
}

// PricingInfo says which prices produced a summary.
type PricingInfo struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256,omitempty"`
	Loaded bool   `json:"loaded"`
}

// Summary is what `ai-usage plan-cost -json` prints.
type Summary struct {
	Version   int    `json:"version"`
	Generated string `json:"generated"`
	Currency  string `json:"currency"`
	// Complete is true only when every requested session was found, every model was priced, and
	// every source read cleanly. Anything else is reported in the fields below, never as zero.
	Complete bool          `json:"complete"`
	Totals   usage.Metrics `json:"totals"`
	// EstimatedCostUSD is null unless the summary is Complete. PricedCostUSD is the sum of the
	// priced models found, so an incomplete summary still shows a lower bound.
	EstimatedCostUSD *float64      `json:"estimated_cost_usd"`
	PricedCostUSD    float64       `json:"priced_cost_usd"`
	Models           []ModelCost   `json:"models"`
	Sessions         []SessionCost `json:"sessions"`
	MissingSessions  []string      `json:"missing_sessions"`
	UnpricedModels   []string      `json:"unpriced_models"`
	SourceErrors     []string      `json:"source_errors"`
	Pricing          PricingInfo   `json:"pricing"`
}

// SessionSource is what Compute needs from an aggregator.
type SessionSource interface {
	SessionRows(withDetail bool) []usage.SessionRow
	SessionModels(k usage.SessionKey) map[string]usage.Metrics
}

// ErrAmbiguous is returned when a bare session id matches sessions in more than one tool/profile.
var ErrAmbiguous = errors.New("ambiguous session id")

// Resolve turns requested session references into keys. A reference is "tool|profile|id" or a
// bare id; a bare id must match exactly one session. Unmatched references are returned as
// missing (normalized, in request order); duplicates are dropped.
func Resolve(rows []usage.SessionRow, refs []string) (keys []usage.SessionKey, missing []string, err error) {
	byKey := map[usage.SessionKey]bool{}
	byID := map[string][]usage.SessionKey{}
	for _, r := range rows {
		byKey[r.Key()] = true
		byID[r.ID] = append(byID[r.ID], r.Key())
	}
	seen := map[usage.SessionKey]bool{}
	seenMissing := map[string]bool{}
	add := func(k usage.SessionKey) {
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	miss := func(ref string) {
		if !seenMissing[ref] {
			seenMissing[ref] = true
			missing = append(missing, ref)
		}
	}
	for _, ref := range refs {
		ref = strings.TrimSpace(ref)
		if ref == "" {
			continue
		}
		if strings.Contains(ref, "|") {
			parts := strings.Split(ref, "|")
			if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
				return nil, nil, fmt.Errorf("session %q: want tool|profile|id or a bare id", ref)
			}
			k := usage.SessionKey{Tool: parts[0], Profile: parts[1], ID: parts[2]}
			if byKey[k] {
				add(k)
			} else {
				miss(k.String())
			}
			continue
		}
		matches := byID[ref]
		switch len(matches) {
		case 0:
			miss(ref)
		case 1:
			add(matches[0])
		default:
			names := make([]string, len(matches))
			for i, k := range matches {
				names[i] = k.String()
			}
			sort.Strings(names)
			return nil, nil, fmt.Errorf("%w %q matches %s; link it as tool|profile|id", ErrAmbiguous, ref, strings.Join(names, ", "))
		}
	}
	return keys, missing, nil
}

// Compute totals the requested sessions. sourceErrors are source statuses that were neither "ok"
// nor "not found"; they make the summary incomplete because a failed read can hide usage.
func Compute(src SessionSource, refs []string, pricing *Pricing, sourceErrors []string, now time.Time) (Summary, error) {
	rows := src.SessionRows(false)
	keys, missing, err := Resolve(rows, refs)
	if err != nil {
		return Summary{}, err
	}
	rowOf := map[usage.SessionKey]usage.SessionRow{}
	for _, r := range rows {
		rowOf[r.Key()] = r
	}
	s := Summary{
		Version: Version, Generated: now.UTC().Format(time.RFC3339), Currency: "USD",
		Models: []ModelCost{}, Sessions: []SessionCost{}, MissingSessions: nonNil(missing),
		UnpricedModels: []string{}, SourceErrors: nonNil(sourceErrors),
	}
	if pricing != nil {
		s.Pricing = PricingInfo{Path: pricing.Path, SHA256: pricing.SHA256, Loaded: pricing.Loaded}
	}
	// Model totals sum the per-session costs instead of repricing the summed counters: fresh input
	// is clamped at zero per session, so repricing an aggregate could disagree with the total.
	allModels := map[string]*usage.Metrics{}
	modelCost := map[string]float64{}
	unpriced := map[string]bool{}
	for _, k := range keys {
		row := rowOf[k]
		sc := SessionCost{Key: k.String(), Tool: k.Tool, Profile: k.Profile, ID: k.ID, Title: row.Title,
			First: row.First, Last: row.Last, Models: []ModelCost{}}
		sessionPriced := true
		for name, m := range src.SessionModels(k) {
			sc.Totals.Add(m)
			mc := ModelCost{Model: name, Metrics: m}
			if price, ok := pricing.Lookup(name); ok {
				c := price.Cost(m)
				mc.CostUSD = ptr(round(c))
				sc.PricedCostUSD += c
				modelCost[name] += c
			} else {
				sessionPriced = false
				unpriced[name] = true
			}
			sc.Models = append(sc.Models, mc)
			acc := allModels[name]
			if acc == nil {
				acc = &usage.Metrics{}
				allModels[name] = acc
			}
			acc.Add(m)
		}
		sortModels(sc.Models)
		s.PricedCostUSD += sc.PricedCostUSD
		sc.PricedCostUSD = round(sc.PricedCostUSD)
		if sessionPriced {
			sc.CostUSD = ptr(sc.PricedCostUSD)
		}
		s.Totals.Add(sc.Totals)
		s.Sessions = append(s.Sessions, sc)
	}
	for name, m := range allModels {
		mc := ModelCost{Model: name, Metrics: *m}
		if !unpriced[name] {
			mc.CostUSD = ptr(round(modelCost[name]))
		}
		s.Models = append(s.Models, mc)
	}
	sortModels(s.Models)
	for name := range unpriced {
		s.UnpricedModels = append(s.UnpricedModels, name)
	}
	sort.Strings(s.UnpricedModels)
	s.PricedCostUSD = round(s.PricedCostUSD)
	s.Complete = len(s.MissingSessions) == 0 && len(s.UnpricedModels) == 0 && len(s.SourceErrors) == 0 &&
		pricing != nil && pricing.Loaded && len(keys) > 0
	if s.Complete {
		s.EstimatedCostUSD = ptr(s.PricedCostUSD)
	}
	return s, nil
}

func sortModels(m []ModelCost) {
	sort.Slice(m, func(i, j int) bool {
		if m[i].Total != m[j].Total {
			return m[i].Total > m[j].Total
		}
		return m[i].Model < m[j].Model
	})
}

// round keeps micro-dollar precision so JSON doesn't carry float noise.
func round(v float64) float64 { return math.Round(v*1e6) / 1e6 }

func ptr(v float64) *float64 { return &v }

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
