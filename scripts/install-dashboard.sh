#!/usr/bin/env bash
# Build ai-usage and run its dashboard as a per-user LaunchAgent on macOS.
#
#   scripts/install-dashboard.sh              # build, (re)install, start; http://127.0.0.1:8790
#   scripts/install-dashboard.sh --port 8795  # another loopback port
#   scripts/install-dashboard.sh --check      # agent state and /healthz
#   scripts/install-dashboard.sh --uninstall  # stop and remove the agent (keeps binary, data, logs)
#
# The agent is labeled <your-username>.ai-usage-dashboard and runs ~/.local/bin/ai-usage, so it
# doesn't depend on where this checkout lives. History stays in ~/.local/share/ai-usage and
# budgets in ~/.config/ai-usage/dashboard.json; an existing <checkout>/data/hourly.json,
# data/work-report/, and dashboard.json are copied there once.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo="$(cd "$here/.." && pwd)"
# shellcheck source-path=SCRIPTDIR
source "$here/launchd.sh"

service="ai-usage-dashboard"
label="$(agent_label "$service")"
plist="$HOME/Library/LaunchAgents/$label.plist"
bin_path="$HOME/.local/bin/ai-usage"
log_dir="$HOME/Library/Logs/ai-usage"
data_dir="${AI_USAGE_DATA_DIR:-$HOME/.local/share/ai-usage}"
config="${AI_USAGE_CONFIG:-$HOME/.config/ai-usage/dashboard.json}"
domain="gui/$(id -u)"
port="${AI_USAGE_PORT:-8790}"
interval=30s
mode=install

die() { printf 'install-dashboard.sh: %s\n' "$*" >&2; exit 1; }

while [[ $# -gt 0 ]]; do
    case "$1" in
        --port) port="${2:?}"; shift 2 ;;
        --interval) interval="${2:?}"; shift 2 ;;
        --check) mode=check; shift ;;
        --uninstall) mode=uninstall; shift ;;
        -h|--help) sed -n '2,13p' "$0"; exit 0 ;;
        *) die "unknown argument: $1" ;;
    esac
done
[[ "$(uname -s)" == Darwin ]] || die "LaunchAgents are macOS only; run 'ai-usage serve' directly."
[[ "$port" =~ ^[0-9]{2,5}$ ]] || die "port must be a number: $port"
[[ "$interval" =~ ^[0-9]+(s|m)$ ]] || die "interval must look like 30s or 2m: $interval"

case "$mode" in
    uninstall)
        launchctl bootout "$domain/$label" 2>/dev/null || true
        rm -f "$plist"
        echo "Removed $label (binary, data, and logs kept)."
        exit 0 ;;
    check)
        if launchctl print "$domain/$label" >/dev/null 2>&1; then
            echo "agent:   loaded ($label)"
            launchctl print "$domain/$label" | grep -E '^\s+(state|pid) =' | sed 's/^\s*/         /'
        else
            echo "agent:   not loaded ($label)"
        fi
        echo "binary:  $bin_path ($("$bin_path" version 2>/dev/null || echo missing))"
        echo "healthz: $(curl -fsS --max-time 3 "http://127.0.0.1:$port/healthz" 2>/dev/null || echo unavailable)"
        echo "url:     http://127.0.0.1:$port"
        exit 0 ;;
esac

command -v go >/dev/null || die "go is not installed"
mkdir -p "$(dirname "$bin_path")" "$log_dir" "$(dirname "$plist")" "$data_dir" "$(dirname "$config")"
chmod 700 "$data_dir"
version="$(git -C "$repo" describe --always --dirty 2>/dev/null || echo dev)"
(cd "$repo" && go build -trimpath -ldflags "-X main.version=$version" -o "$bin_path" ./cmd/ai-usage)
echo "Built $bin_path ($version)"

# One-time copy of state that used to live in the checkout.
[[ -f "$repo/data/hourly.json" && ! -f "$data_dir/hourly.json" ]] && cp "$repo/data/hourly.json" "$data_dir/" && echo "Copied history to $data_dir/hourly.json"
[[ -d "$repo/data/work-report" && ! -e "$data_dir/work-report" ]] && cp -R "$repo/data/work-report" "$data_dir/" && echo "Copied work report to $data_dir/work-report"
[[ -f "$repo/dashboard.json" && ! -f "$config" ]] && cp "$repo/dashboard.json" "$config" && echo "Copied budgets to $config"

retire_other_agents "$service" "$label"

cat > "$plist.tmp" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key><string>$(xml_escape "$label")</string>
    <key>ProgramArguments</key>
    <array>
        <string>$(xml_escape "$bin_path")</string>
        <string>serve</string>
        <string>-port</string><string>$port</string>
        <string>-interval</string><string>$interval</string>
        <string>-data-dir</string><string>$(xml_escape "$data_dir")</string>
        <string>-config</string><string>$(xml_escape "$config")</string>
    </array>
    <key>EnvironmentVariables</key>
    <dict><key>PATH</key><string>/usr/bin:/bin:/usr/sbin:/sbin</string></dict>
    <key>RunAtLoad</key><true/>
    <key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
    <key>ThrottleInterval</key><integer>30</integer>
    <key>ProcessType</key><string>Background</string>
    <key>LowPriorityIO</key><true/>
    <key>Nice</key><integer>10</integer>
    <key>StandardOutPath</key><string>$(xml_escape "$log_dir/dashboard.log")</string>
    <key>StandardErrorPath</key><string>$(xml_escape "$log_dir/dashboard.log")</string>
</dict>
</plist>
PLIST
plutil -lint "$plist.tmp" >/dev/null || die "generated plist is invalid: $plist.tmp"
mv "$plist.tmp" "$plist"

launchctl bootout "$domain/$label" 2>/dev/null || true
launchctl bootstrap "$domain" "$plist"
echo "Loaded $label; logs: $log_dir/dashboard.log"
for _ in $(seq 1 40); do
    curl -fsS --max-time 1 "http://127.0.0.1:$port/healthz" >/dev/null 2>&1 && { echo "Dashboard: http://127.0.0.1:$port"; exit 0; }
    sleep 0.5
done
die "agent loaded but /healthz did not answer on port $port; see $log_dir/dashboard.log"
