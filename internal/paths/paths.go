// Package paths holds ai-usage's default file locations. They don't depend on where the checkout
// lives, so moving or re-cloning the repository keeps the same history.
package paths

import (
	"os"
	"path/filepath"

	"ai-usage/internal/usage"
)

// DefaultPort is the dashboard's loopback port.
const DefaultPort = 8790

// DataDir holds history (hourly.json) and the ai-work-report overlay: $AI_USAGE_DATA_DIR, else
// ~/.local/share/ai-usage.
func DataDir() string {
	if d := os.Getenv("AI_USAGE_DATA_DIR"); d != "" {
		return usage.ExpandHome(d)
	}
	return filepath.Join(usage.Home, ".local", "share", "ai-usage")
}

// ConfigFile is the optional budgets file: $AI_USAGE_CONFIG, else ~/.config/ai-usage/dashboard.json.
func ConfigFile() string {
	if f := os.Getenv("AI_USAGE_CONFIG"); f != "" {
		return usage.ExpandHome(f)
	}
	return filepath.Join(usage.Home, ".config", "ai-usage", "dashboard.json")
}

// ProjectsFile holds the ownership rules: $AI_USAGE_PROJECTS, else
// ~/.config/ai-usage/projects.yaml.
func ProjectsFile() string {
	if f := os.Getenv("AI_USAGE_PROJECTS"); f != "" {
		return usage.ExpandHome(f)
	}
	return filepath.Join(usage.Home, ".config", "ai-usage", "projects.yaml")
}

// HistoryFile is the hourly history store inside a data dir.
func HistoryFile(dataDir string) string { return filepath.Join(dataDir, "hourly.json") }

// WorkReportDir is the ai-work-report workdir inside a data dir.
func WorkReportDir(dataDir string) string { return filepath.Join(dataDir, "work-report") }
