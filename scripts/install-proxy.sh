#!/usr/bin/env bash
# Build foundry-entra-proxy and run it as a per-user LaunchAgent on macOS.
#
#   scripts/install-proxy.sh               # build, create the client key if missing, (re)load the agent
#   scripts/install-proxy.sh --check       # report binary, key, agent, and /healthz + /readyz state
#   scripts/install-proxy.sh --uninstall   # unload the agent and remove its plist (keeps binary, key, logs)
#
# The agent is labeled <your-username>.foundry-entra-proxy.
#
# Settings (flag, then environment, then default):
#   --resource    FEP_RESOURCE, ANTHROPIC_FOUNDRY_RESOURCE   Foundry resource name (required)
#   --deployments FEP_ALLOWED_DEPLOYMENTS                     default: gpt-6-luna,gpt-6.1-sol,gpt-6-astra
#   --tenant      FEP_TENANT                                  optional Entra tenant ID
#   --listen      FEP_LISTEN                                  default: 127.0.0.1:8787
set -Eeuo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo="$(cd "$here/.." && pwd)"
# shellcheck source-path=SCRIPTDIR
source "$here/launchd.sh"
service="foundry-entra-proxy"
label="$(agent_label "$service")"
bin_path="$HOME/.local/bin/foundry-entra-proxy"
key_dir="$HOME/.config/foundry-entra-proxy"
key_file="$key_dir/client.key"
log_dir="$HOME/Library/Logs/foundry-entra-proxy"
plist="$HOME/Library/LaunchAgents/$label.plist"
domain="gui/$(id -u)"

resource="${FEP_RESOURCE:-${ANTHROPIC_FOUNDRY_RESOURCE:-}}"
deployments="${FEP_ALLOWED_DEPLOYMENTS:-gpt-6-luna,gpt-6.1-sol,gpt-6-astra}"
tenant="${FEP_TENANT:-}"
default_listen="127.0.0.1:8787"
listen="${FEP_LISTEN:-$default_listen}"
listen_from_user=0
[[ -z "${FEP_LISTEN:-}" ]] || listen_from_user=1
mode=install

die() { printf 'install-proxy.sh: %s\n' "$*" >&2; exit 1; }

# usage prints the comment block at the top of this file, without the leading "# ".
usage() { awk 'NR == 1 { next } /^#/ { sub(/^# ?/, ""); print; next } { exit }' "$0"; }

while [[ $# -gt 0 ]]; do
    case "$1" in
        --resource) resource="${2:?}"; shift 2 ;;
        --deployments) deployments="${2:?}"; shift 2 ;;
        --tenant) tenant="${2:?}"; shift 2 ;;
        --listen) listen="${2:?}"; listen_from_user=1; shift 2 ;;
        --check) mode=check; shift ;;
        --uninstall) mode=uninstall; shift ;;
        -h|--help) usage; exit 0 ;;
        *) die "unknown argument: $1" ;;
    esac
done

[[ "$(uname -s)" == Darwin ]] || die "LaunchAgents are macOS only; run the binary directly elsewhere (see README)."

probe() {
    local path="$1"
    curl -fsS --max-time 40 "http://$listen$path" 2>/dev/null || printf 'unavailable'
}

if [[ "$mode" == uninstall ]]; then
    launchctl bootout "$domain/$label" 2>/dev/null || true
    rm -f "$plist"
    echo "Unloaded $label and removed $plist (binary, key, and logs kept)."
    exit 0
fi

if [[ "$mode" == check ]]; then
    status=0
    # Without an explicit --listen or FEP_LISTEN, probe the address the installed agent uses.
    if ((listen_from_user == 0)) && [[ -f "$plist" ]]; then
        if installed_listen="$(/usr/libexec/PlistBuddy -c 'Print :EnvironmentVariables:FEP_LISTEN' "$plist" 2>/dev/null)" \
            && [[ "$installed_listen" =~ ^(127\.0\.0\.1|\[::1\]|localhost):[0-9]+$ ]]; then
            listen="$installed_listen"
        fi
    fi
    echo "listen:  $listen"
    if [[ -x "$bin_path" ]]; then
        echo "binary:  $bin_path ($("$bin_path" -version))"
    else
        echo "binary:  missing ($bin_path)"; status=2
    fi
    if [[ -f "$key_file" && "$(stat -f %Lp "$key_file")" == 600 ]]; then
        echo "key:     $key_file (mode 600)"
    else
        echo "key:     missing or not mode 600 ($key_file)"; status=2
    fi
    if launchctl print "$domain/$label" >/dev/null 2>&1; then
        echo "agent:   loaded ($label)"
    else
        echo "agent:   not loaded ($label)"; status=2
    fi
    echo "healthz: $(probe /healthz)"
    echo "readyz:  $(probe /readyz)"
    exit "$status"
fi

# install
[[ -n "$resource" ]] || die "set --resource or ANTHROPIC_FOUNDRY_RESOURCE (the Foundry resource name)."
[[ "$resource" =~ ^[a-z0-9][a-z0-9-]{1,62}$ ]] || die "resource name looks wrong: $resource"
[[ "$deployments" =~ ^[A-Za-z0-9._-]+(,[A-Za-z0-9._-]+)*$ ]] || die "deployments must be a comma-separated list: $deployments"
[[ -z "$tenant" || "$tenant" =~ ^[0-9a-fA-F-]{36}$ ]] || die "tenant must be a GUID: $tenant"
[[ "$listen" =~ ^(127\.0\.0\.1|\[::1\]|localhost):[0-9]+$ ]] || die "listen must be a loopback address: $listen"
command -v go >/dev/null || die "go is not installed"
az_path="$(command -v az)" || die "az is not installed"

mkdir -p "$(dirname "$bin_path")" "$log_dir" "$(dirname "$plist")"
version="$(git -C "$repo" rev-parse --short HEAD 2>/dev/null || echo dev)"
(cd "$repo" && go build -trimpath -ldflags "-X main.version=$version" -o "$bin_path" ./cmd/foundry-entra-proxy)
echo "Built $bin_path ($version)"

if [[ ! -f "$key_file" ]]; then
    mkdir -p "$key_dir"
    chmod 700 "$key_dir"
    (umask 077; openssl rand -hex 32 > "$key_file")
    echo "Created client key $key_file"
fi
chmod 600 "$key_file"

retire_other_agents "$service" "$label"

# Values were validated above; the label and paths are escaped because they come from the account.
tenant_xml=""
[[ -z "$tenant" ]] || tenant_xml="<key>FEP_TENANT</key><string>$tenant</string>"
cat > "$plist.tmp" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key><string>$(xml_escape "$label")</string>
    <key>ProgramArguments</key><array><string>$(xml_escape "$bin_path")</string></array>
    <key>EnvironmentVariables</key>
    <dict>
        <key>FEP_UPSTREAM_BASE</key><string>https://$resource.services.ai.azure.com</string>
        <key>FEP_CLIENT_KEY_FILE</key><string>$(xml_escape "$key_file")</string>
        <key>FEP_ALLOWED_DEPLOYMENTS</key><string>$deployments</string>
        <key>FEP_LISTEN</key><string>$listen</string>
        <key>FEP_AZ_PATH</key><string>$(xml_escape "$az_path")</string>
        <key>PATH</key><string>$(xml_escape "$(dirname "$az_path")"):/usr/bin:/bin:/usr/sbin:/sbin</string>
        $tenant_xml
    </dict>
    <key>RunAtLoad</key><true/>
    <key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
    <key>ThrottleInterval</key><integer>30</integer>
    <key>ProcessType</key><string>Background</string>
    <key>StandardOutPath</key><string>$(xml_escape "$log_dir/proxy.log")</string>
    <key>StandardErrorPath</key><string>$(xml_escape "$log_dir/proxy.log")</string>
</dict>
</plist>
EOF
plutil -lint "$plist.tmp" >/dev/null || die "generated plist is invalid: $plist.tmp"
mv "$plist.tmp" "$plist"
chmod 644 "$plist"

# bootout returns before the old instance has finished tearing down, and bootstrapping while it
# is still registered fails with "Bootstrap failed: 5: Input/output error". Wait for it to go.
if launchctl print "$domain/$label" >/dev/null 2>&1; then
    launchctl bootout "$domain/$label" 2>/dev/null || true
    for _ in $(seq 1 50); do
        launchctl print "$domain/$label" >/dev/null 2>&1 || break
        sleep 0.2
    done
    if launchctl print "$domain/$label" >/dev/null 2>&1; then
        echo "install-proxy.sh: warning: $label still registered 10s after bootout; trying bootstrap anyway" >&2
    fi
fi
launchctl bootstrap "$domain" "$plist" \
    || die "launchctl bootstrap failed; $label is NOT loaded and the proxy is down. Retry scripts/install-proxy.sh or run: launchctl bootstrap $domain $plist"
echo "Loaded $label ($plist); logs: $log_dir/proxy.log"

healthy=0
for _ in $(seq 1 20); do
    if [[ "$(probe /healthz)" == *ok* ]]; then
        healthy=1
        break
    fi
    sleep 0.5
done
((healthy == 1)) || die "proxy did not report ok on http://$listen/healthz within 10s; check $log_dir/proxy.log"
echo "healthz: ok"
echo "readyz:  $(probe /readyz)"
