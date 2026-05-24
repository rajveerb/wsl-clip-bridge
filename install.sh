#!/usr/bin/env bash
# install.sh — build clip-listener.exe and install the WSL bridge.

set -euo pipefail

WITH_BASHRC_AUTOSTART=0
WITH_WSL_BOOT_AUTOSTART=0
WITH_KEYBINDING=0
for arg in "$@"; do
    case "$arg" in
        --with-autostart)  WITH_BASHRC_AUTOSTART=1 ;;
        --with-wsl-boot)   WITH_WSL_BOOT_AUTOSTART=1 ;;
        --with-keybinding) WITH_KEYBINDING=1 ;;
        --help|-h)
            cat <<'USAGE'
Usage: install.sh [--with-autostart] [--with-keybinding] [--with-wsl-boot]

  --with-autostart   Add a snippet to ~/.bashrc so the bridge starts in
                     the background the first time a WSL shell opens.
                     No sudo. Idempotent.

  --with-keybinding  Write ~/.claude/keybindings.json with
                     alt+v -> chat:imagePaste so Claude Code can paste
                     images on a key Windows Terminal does not eat.
                     Refuses to overwrite an existing keybindings.json.

  --with-wsl-boot    Edit /etc/wsl.conf [boot] command so the bridge
                     starts at WSL boot before any shell opens.
                     Requires sudo. Heavier — only worth it if you need
                     the bridge running outside any shell.

For a turnkey WSL + Claude Code setup, use:
  ./install.sh --with-autostart --with-keybinding

Without any flag, the script only builds and installs the binaries.
USAGE
            exit 0
            ;;
        *)
            echo "Unknown arg: $arg" >&2
            echo "See: install.sh --help" >&2
            exit 2
            ;;
    esac
done

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
INSTALL_BIN="$HOME/.local/bin"
INSTALL_SHARE="$HOME/.local/share/wsl-clip-bridge"

configure_claude_keybinding() {
    local kb="$HOME/.claude/keybindings.json"

    echo
    echo "Configuring Claude Code keybinding in $kb"
    mkdir -p "$(dirname "$kb")"

    if [[ -f "$kb" ]]; then
        if grep -q '"chat:imagePaste"' "$kb" 2>/dev/null && grep -q '"alt+v"' "$kb" 2>/dev/null; then
            echo "  alt+v -> chat:imagePaste already in $kb — no change"
            return 0
        fi
        echo "  $kb exists with other content; refusing to overwrite." >&2
        echo "  Manually add to its Chat context bindings:" >&2
        echo '    "alt+v": "chat:imagePaste",' >&2
        echo '    "ctrl+alt+v": "chat:imagePaste"' >&2
        return 1
    fi

    cat >"$kb" <<'KBJSON'
{
  "$schema": "https://www.schemastore.org/claude-code-keybindings.json",
  "$docs": "https://code.claude.com/docs/en/keybindings",
  "bindings": [
    {
      "context": "Chat",
      "bindings": {
        "alt+v": "chat:imagePaste",
        "ctrl+alt+v": "chat:imagePaste"
      }
    }
  ]
}
KBJSON
    echo "  Wrote $kb (alt+v and ctrl+alt+v -> chat:imagePaste)"
    echo "  Active in new Claude Code sessions; restart Claude Code if it is running."
}

configure_bashrc_autostart() {
    local rc="$HOME/.bashrc"
    local marker="# wsl-clip-bridge autostart"

    echo
    echo "Configuring autostart in $rc"

    if grep -qF "$marker" "$rc" 2>/dev/null; then
        echo "  Snippet already present — no change."
        return 0
    fi

    local ts
    ts="$(date +%Y%m%d-%H%M%S)"
    if [[ -e "$rc" ]]; then
        cp "$rc" "${rc}.bak.${ts}"
        echo "  Backup: ${rc}.bak.${ts}"
    fi

    cat >>"$rc" <<'BASHRC'

# wsl-clip-bridge autostart
if [ -n "$WAYLAND_DISPLAY" ] && command -v wsl-clip-bridge >/dev/null 2>&1; then
    if ! wsl-clip-bridge --status 2>/dev/null | grep -q running; then
        nohup wsl-clip-bridge >/dev/null 2>&1 &
        disown
    fi
fi
BASHRC
    echo "  Appended snippet to $rc"
    echo
    echo "Active in new shells. To start in this shell:"
    echo "  nohup wsl-clip-bridge >/dev/null 2>&1 & disown"
}

configure_wslconf_autostart() {
    local wslconf="/etc/wsl.conf"
    local user="${USER:-$(whoami)}"
    local bridge_path="$INSTALL_BIN/wsl-clip-bridge"
    local cmd_value="su - $user -c 'nohup $bridge_path >/dev/null 2>&1 &'"
    local new_line="command = $cmd_value"

    echo
    echo "Configuring WSL autostart in $wslconf"
    echo "  Adding: $new_line"
    echo "  (sudo required)"

    local current=""
    if sudo test -e "$wslconf"; then
        current="$(sudo cat "$wslconf")"
    fi

    if grep -qF "$bridge_path" <<<"$current"; then
        echo "  Already configured — no change."
        return 0
    fi

    local existing_cmd
    existing_cmd="$(grep -E '^[[:space:]]*command[[:space:]]*=' <<<"$current" || true)"
    if [[ -n "$existing_cmd" ]]; then
        echo "ERROR: $wslconf already defines a [boot] command:" >&2
        echo "  $existing_cmd" >&2
        echo "Merge manually by chaining the commands with ';' or '&'." >&2
        return 1
    fi

    local new_content
    if [[ -z "$current" ]]; then
        new_content=$'[boot]\n'"$new_line"
    elif grep -qE '^\[boot\]' <<<"$current"; then
        new_content="$(awk -v line="$new_line" '
            BEGIN { inserted = 0 }
            /^\[boot\]/ && !inserted { print; print line; inserted = 1; next }
            { print }
        ' <<<"$current")"
    else
        new_content="$current"$'\n\n[boot]\n'"$new_line"
    fi

    local ts
    ts="$(date +%Y%m%d-%H%M%S)"
    if sudo test -e "$wslconf"; then
        sudo cp "$wslconf" "${wslconf}.bak.${ts}"
        echo "  Backup: ${wslconf}.bak.${ts}"
    fi
    printf '%s\n' "$new_content" | sudo tee "$wslconf" >/dev/null
    echo "  Wrote $wslconf"
    echo
    echo "Apply with (run in Windows PowerShell):"
    echo "  wsl --shutdown"
    echo "Then reopen this WSL terminal."
}

# ----- Locate Go -----
GO_BIN="${GO_BIN:-}"
if [[ -z "$GO_BIN" ]]; then
    if command -v go >/dev/null 2>&1; then
        GO_BIN="$(command -v go)"
    elif [[ -x "$HOME/.local/go/bin/go" ]]; then
        GO_BIN="$HOME/.local/go/bin/go"
    else
        echo "Go not found. Install it (e.g. to ~/.local/go) or set GO_BIN=/path/to/go" >&2
        exit 1
    fi
fi
echo "Using Go: $GO_BIN ($("$GO_BIN" version))"

# ----- Build the listener -----
echo "Building clip-listener.exe ..."
cd "$REPO_ROOT/windows/clip-listener"
GOOS=windows GOARCH=amd64 "$GO_BIN" build \
    -trimpath \
    -ldflags="-s -w" \
    -o clip-listener.exe .
ls -lh clip-listener.exe

# ----- Install -----
mkdir -p "$INSTALL_BIN" "$INSTALL_SHARE"
install -m 0755 clip-listener.exe                  "$INSTALL_SHARE/clip-listener.exe"
install -m 0755 "$REPO_ROOT/wsl/wsl-clip-bridge"   "$INSTALL_BIN/wsl-clip-bridge"

# ----- Prereq checks -----
if ! command -v wl-copy >/dev/null 2>&1; then
    echo
    echo "wl-copy not found. Install with:"
    echo "  sudo apt install wl-clipboard"
fi

case ":$PATH:" in
    *":$INSTALL_BIN:"*) ;;
    *)
        echo
        echo "WARNING: $INSTALL_BIN is not on PATH."
        echo "Add to ~/.bashrc:  export PATH=\"\$HOME/.local/bin:\$PATH\""
        ;;
esac

# ----- Optional autostart / keybinding -----
if [[ $WITH_BASHRC_AUTOSTART -eq 1 ]]; then
    configure_bashrc_autostart
fi
if [[ $WITH_KEYBINDING -eq 1 ]]; then
    configure_claude_keybinding
fi
if [[ $WITH_WSL_BOOT_AUTOSTART -eq 1 ]]; then
    configure_wslconf_autostart
fi

cat <<EOF

Installed:
  $INSTALL_SHARE/clip-listener.exe
  $INSTALL_BIN/wsl-clip-bridge

Start in this shell (foreground, ^C to stop the bash side):
  wsl-clip-bridge

Or backgrounded:
  nohup wsl-clip-bridge >/dev/null 2>&1 & disown

EOF

if [[ $WITH_BASHRC_AUTOSTART -eq 0 || $WITH_KEYBINDING -eq 0 ]]; then
    cat <<'EOF'
Tip: for a turnkey setup, re-run with:
  ./install.sh --with-autostart --with-keybinding
EOF
fi

cat <<'EOF'

Verify:
  1. Copy any image in Windows (Win+Shift+S)
  2. wl-paste -l                # should list image/png
  3. In Claude Code, press Alt+V  (or Ctrl+Alt+V) to paste the image
     (If you have unbound Ctrl+V in Windows Terminal, Ctrl+V also works.)
EOF
