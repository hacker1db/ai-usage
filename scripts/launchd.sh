#!/usr/bin/env bash
# Shared LaunchAgent helpers for the installers. Source it; don't run it.
#
# Labels are "<user>.<service>", where <user> is the account running the installer with any
# character launchd labels don't allow replaced by "-" (jdoe@example.com -> jdoe-example.com).

# agent_label SERVICE -> prints the per-user label, e.g. jdoe.foundry-entra-proxy
agent_label() {
    local user
    user="$(id -un)"
    user="${user//[^A-Za-z0-9.-]/-}"
    printf '%s.%s' "$user" "$1"
}

# retire_other_agents SERVICE CURRENT_LABEL
# Unloads and removes any other ~/Library/LaunchAgents/*.SERVICE.plist, such as one installed
# under an older naming scheme, so only one copy of the service runs.
retire_other_agents() {
    local service="$1" current="$2" plist label
    shopt -s nullglob
    for plist in "$HOME/Library/LaunchAgents/"*".$service.plist"; do
        label="$(plutil -extract Label raw -o - "$plist" 2>/dev/null || basename "$plist" .plist)"
        [[ "$label" == "$current" ]] && continue
        launchctl bootout "gui/$(id -u)/$label" 2>/dev/null || true
        rm -f "$plist"
        echo "Retired older agent $label ($plist)"
    done
    shopt -u nullglob
}

# xml_escape VALUE -> VALUE safe inside a plist <string>
xml_escape() {
    local s="$1"
    s="${s//&/&amp;}"; s="${s//</&lt;}"; s="${s//>/&gt;}"
    printf '%s' "$s"
}
