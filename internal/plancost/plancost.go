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

const Version = 1

const DefaultPricingPath = "~/.config/ai-usage/pricing.json"

type Price struct {
	Input       float64 `json:"input"`
	CachedInput float64 `json:"cached_input"`
	CacheWrite  float64 `json:"cache_write"`
	Output      float64 `json:"output"`
}

type Pricing struct {
	Path   string
	SHA256 string
	Loaded bool
	exact  map[string]Price
	prefix []prefixPrice
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

func (p Price) Cost(m usage.Metrics) float64 {
	fresh := max(m.Input-m.Cached-m.CacheWrite, 0)
	return (float64(fresh)*p.Input + float64(m.Cached)*p.CachedInput +
		float64(m.CacheWrite)*p.CacheWrite + float64(m.Output)*p.Output) / 1e6
}

type ModelCost struct {
	Model string `json:"model"`
	usage.Metrics
	CostUSD *float64 `json:"cost_usd"`
}

type SessionCost struct {
	Key     string        `json:"key"`
	Tool    string        `json:"tool"`
	Profile string        `json:"profile"`
	ID      string        `json:"id"`
	Title   string        `json:"title"`
	First   string        `json:"first"`
	Last    string        `json:"last"`
	Totals  usage.Metrics `json:"totals"`

	CostUSD       *float64    `json:"cost_usd"`
	PricedCostUSD float64     `json:"priced_cost_usd"`
	Models        []ModelCost `json:"models"`
}

type PricingInfo struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256,omitempty"`
	Loaded bool   `json:"loaded"`
}

type Summary struct {
	Version   int    `json:"version"`
	Generated string `json:"generated"`
	Currency  string `json:"currency"`

	Complete bool          `json:"complete"`
	Totals   usage.Metrics `json:"totals"`

	EstimatedCostUSD *float64      `json:"estimated_cost_usd"`
	PricedCostUSD    float64       `json:"priced_cost_usd"`
	Models           []ModelCost   `json:"models"`
	Sessions         []SessionCost `json:"sessions"`
	MissingSessions  []string      `json:"missing_sessions"`
	UnpricedModels   []string      `json:"unpriced_models"`
	SourceErrors     []string      `json:"source_errors"`
	Pricing          PricingInfo   `json:"pricing"`
}

type SessionSource interface {
	SessionRows(withDetail bool) []usage.SessionRow
	SessionModels(k usage.SessionKey) map[string]usage.Metrics
}

var ErrAmbiguous = errors.New("ambiguous session id")

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

func round(v float64) float64 { return math.Round(v*1e6) / 1e6 }

func ptr(v float64) *float64 { return &v }

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
