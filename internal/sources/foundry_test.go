package sources

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestFoundryEndpoints(t *testing.T) {
	t.Setenv("ANTHROPIC_FOUNDRY_RESOURCE", "")
	t.Setenv("ANTHROPIC_FOUNDRY_BASE_URL", "https://env-res.services.ai.azure.com/anthropic")
	dir := t.TempDir()
	claude := filepath.Join(dir, "claude")
	codex := filepath.Join(dir, "codex")
	for _, d := range []string{claude, codex} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p, s string) {
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(claude, "settings.json"), `{"env":{"ANTHROPIC_FOUNDRY_RESOURCE":"Example-Foundry","OTHER":"x"}}`)
	write(filepath.Join(codex, "config.toml"), "[model_providers.foundry]\nbase_url = \"https://codex-res.openai.azure.com/openai/v1\"\n"+
		"[model_providers.other]\nbase_url = \"https://example.com/v1\"\n")
	got := FoundryEndpoints(Options{Claude: []ClaudeDir{{claude, "work-foundry"}}, CodexHome: codex})
	if want := []string{"codex-res", "env-res", "example-foundry"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}
