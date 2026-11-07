#!/usr/bin/env bash
# Run the ai-usage dashboard as a per-user LaunchAgent on macOS.
#
#   ./install-dashboard.sh              # (re)install and start; http://127.0.0.1:8790
#   ./install-dashboard.sh --port 8795  # another loopback port
#   ./install-dashboard.sh --check      # agent state and /healthz
#   ./install-dashboard.sh --uninstall  # stop and remove the agent (keeps logs)
#
# The agent runs dashboard.py from this directory, so rerun the script if you move the checkout.
set -euo pipefail

label="dev.hacker1db.ai-usage-dashboard"
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
plist="$HOME/Library/LaunchAgents/$label.plist"
log_dir="$HOME/Library/Logs/ai-usage"
domain="gui/$(id -u)"
port="${AI_USAGE_PORT:-8790}"
interval=30
mode=install

die() { printf 'install-dashboard.sh: %s\n' "$*" >&2; exit 1; }

while [[ $# -gt 0 ]]; do
    case "$1" in
        --port) port="${2:?}"; shift 2 ;;
        --interval) interval="${2:?}"; shift 2 ;;
        --check) mode=check; shift ;;
        --uninstall) mode=uninstall; shift ;;
        -h|--help) sed -n '2,9p' "$0"; exit 0 ;;
        *) die "unknown argument: $1" ;;
    esac
done
[[ "$(uname -s)" == Darwin ]] || die "LaunchAgents are macOS only; run python3 -I dashboard.py directly."
[[ "$port" =~ ^[0-9]{2,5}$ ]] || die "port must be a number: $port"
[[ "$interval" =~ ^[0-9]+$ ]] || die "interval must be seconds: $interval"

case "$mode" in
    uninstall)
        launchctl bootout "$domain/$label" 2>/dev/null || true
        rm -f "$plist"
        echo "Removed $label (logs kept in $log_dir)."
        exit 0 ;;
    check)
        if launchctl print "$domain/$label" >/dev/null 2>&1; then
            echo "agent:   loaded ($label)"
            launchctl print "$domain/$label" | grep -E '^\s+(state|pid) =' | sed 's/^\s*/         /'
        else
            echo "agent:   not loaded ($label)"
        fi
        echo "healthz: $(curl -fsS --max-time 3 "http://127.0.0.1:$port/healthz" 2>/dev/null || echo unavailable)"
        echo "url:     http://127.0.0.1:$port"
        exit 0 ;;
esac

python="$(command -v python3)" || die "python3 is not installed"
[[ -f "$here/dashboard.py" && -f "$here/collect.py" ]] || die "run from the ai-usage checkout"
mkdir -p "$log_dir" "$(dirname "$plist")"

# Paths can contain characters that need escaping in XML.
xml() { local s="$1"; s="${s//&/&amp;}"; s="${s//</&lt;}"; s="${s//>/&gt;}"; printf '%s' "$s"; }
cat > "$plist.tmp" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key><string>$label</string>
    <key>ProgramArguments</key>
    <array>
        <string>$(xml "$python")</string>
        <string>-I</string>
        <string>$(xml "$here/dashboard.py")</string>
        <string>--port</string><string>$port</string>
        <string>--interval</string><string>$interval</string>
    </array>
    <key>WorkingDirectory</key><string>$(xml "$here")</string>
    <key>RunAtLoad</key><true/>
    <key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
    <key>ThrottleInterval</key><integer>30</integer>
    <key>ProcessType</key><string>Background</string>
    <key>LowPriorityIO</key><true/>
    <key>Nice</key><integer>10</integer>
    <key>StandardOutPath</key><string>$(xml "$log_dir/dashboard.log")</string>
    <key>StandardErrorPath</key><string>$(xml "$log_dir/dashboard.log")</string>
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
