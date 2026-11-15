package sources

import (
	"path/filepath"
	"testing"
	"time"

	"ai-usage/internal/usage"
)

func TestCollectGateway(t *testing.T) {
	ts := time.Now().UTC().Format(time.RFC3339Nano)
	path := filepath.Join(t.TempDir(), "gateway.log")
	write(t, path,
		`{"time":"`+ts+`","level":"INFO","msg":"listening","addr":"127.0.0.1:8790"}`,
		`{"time":"`+ts+`","msg":"request","client":"caido","model":"gpt-fast","provider":"foundry","upstream_model":"gpt-6-luna","status":200,"input_tokens":120,"output_tokens":30,"cached_tokens":20,"reasoning_tokens":5,"total_tokens":150}`,
		`{"time":"`+ts+`","msg":"request","client":"caido","model":"sonnet","provider":"anthropic","upstream_model":"claude-sonnet-5-5","status":200,"input_tokens":50,"output_tokens":10,"cached_tokens":30,"cache_write_tokens":5,"total_tokens":60}`,
		`{"time":"`+ts+`","msg":"request","client":"caido","model":"sonnet","provider":"anthropic","status":401}`,
		`{"time":"`+ts+`","msg":"request","model":"gpt-6-luna","status":200,"input_tokens":1}`,
		`not json`,
	)
	agg := usage.NewAggregator(time.Now().Add(-time.Hour), usage.Daily)
	st := CollectGateway(agg, path)
	if st.Status != "ok" || st.Requests != 2 || st.Source != usage.ToolGateway {
		t.Fatalf("status: %+v", st)
	}
	got := map[string]usage.Metrics{}
	for _, r := range agg.Rows() {
		if r.Tool != usage.ToolGateway {
			t.Fatalf("unexpected tool %q", r.Tool)
		}
		got[r.Profile+"/"+r.Model] = r.Metrics
	}
	if m := got["foundry/gpt-6-luna"]; m.Input != 120 || m.Output != 30 || m.Cached != 20 || m.Reasoning != 5 || m.Total != 150 {
		t.Fatalf("foundry row: %+v", m)
	}
	if m := got["anthropic/claude-sonnet-5-5"]; m.Input != 50 || m.CacheWrite != 5 || m.Cached != 30 {
		t.Fatalf("anthropic row: %+v", m)
	}
}

func TestCollectGatewayMissingLog(t *testing.T) {
	st := CollectGateway(usage.NewAggregator(time.Time{}, usage.Daily), filepath.Join(t.TempDir(), "none.log"))
	if st.Status == "ok" {
		t.Fatalf("missing log reported ok: %+v", st)
	}
}
