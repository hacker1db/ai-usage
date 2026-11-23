package paths

import (
	"os"
	"path/filepath"

	"ai-usage/internal/usage"
)

const DefaultPort = 8790

func DataDir() string {
	if d := os.Getenv("AI_USAGE_DATA_DIR"); d != "" {
		return usage.ExpandHome(d)
	}
	return filepath.Join(usage.Home, ".local", "share", "ai-usage")
}

func ConfigFile() string {
	if f := os.Getenv("AI_USAGE_CONFIG"); f != "" {
		return usage.ExpandHome(f)
	}
	return filepath.Join(usage.Home, ".config", "ai-usage", "dashboard.json")
}

func ProjectsFile() string {
	if f := os.Getenv("AI_USAGE_PROJECTS"); f != "" {
		return usage.ExpandHome(f)
	}
	return filepath.Join(usage.Home, ".config", "ai-usage", "projects.yaml")
}

func BillingCacheFile(dataDir string) string { return filepath.Join(dataDir, "billing.json") }

func HistoryFile(dataDir string) string { return filepath.Join(dataDir, "hourly.json") }

func WorkReportDir(dataDir string) string { return filepath.Join(dataDir, "work-report") }
