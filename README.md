# wsl-clip-bridge

Paste images from the Windows clipboard into [Claude Code](https://github.com/anthropics/claude-code)
(or any other Linux app reading the Wayland clipboard) when it's running
inside WSL launched from Windows Terminal.

**Blog post:** [Why Ctrl+V won't paste images in Claude Code on WSL, with a fix](https://rajveerb.com/blog/2026/05/24/on-the-difficulty-of-pasting-a-picture/)

## Why this exists

Out of the box, copying an image in Windows (e.g. `Win+Shift+S`) and trying
to paste it into a Claude Code session inside WSL does nothing. Three reasons:

1. **WSLg only forwards Windows → Linux images as `image/bmp`**
   ([microsoft/wslg#833](https://github.com/microsoft/wslg/issues/833)),
   and those BMPs are BI_BITFIELDS-encoded, which Claude Code's decoder
   cannot read ([anthropics/claude-code#50552](https://github.com/anthropics/claude-code/issues/50552)).
2. **WSLg's reverse sync silently overwrites any PNG you put on the
   Linux clipboard** moments later — and the overwrite produces no
   Windows-side event you can hook.
3. **Windows Terminal grabs `Ctrl+V`** at the terminal layer to paste
   text, so the keystroke never reaches Claude Code's TUI and its
   `chat:imagePaste` handler never fires.

This repo fixes all three: an event-driven Win32 listener encodes the
clipboard image as PNG, a bash bridge pushes it onto the WSL Wayland
clipboard and re-asserts once after WSLg's overwrite, and an `alt+v`
keybinding sidesteps the Windows Terminal Ctrl+V intercept.

## Quick start

```bash
git clone https://github.com/rajveerb/wsl-clip-bridge.git
cd wsl-clip-bridge
sudo apt install wl-clipboard
./install.sh --with-autostart --with-keybinding
```

Open a new WSL shell, snip an image in Windows (`Win+Shift+S`), and press
**`Alt+V`** in Claude Code. The image attaches.

`./install.sh --help` shows all flags.

## Step-by-step setup

If you'd rather see exactly what's happening (or run the steps by hand
instead of via `install.sh`), here is the full procedure.

### 1. Prerequisites

- **WSL2 with WSLg.** Windows 11 ships with this; on Windows 10 you need
  a recent WSL update (`wsl --update`).
- **Go 1.20+ on the Linux side** (for cross-compiling the Windows
  binary). If you don't have it: download from
  [go.dev/dl](https://go.dev/dl/) — no sudo needed if you extract into
  `~/.local/go`.
- **`wl-clipboard`:**
  ```bash
  sudo apt install wl-clipboard
  ```

### 2. Build the Windows listener

The listener is `windows/clip-listener/main.go`. Cross-compile it for
Windows from inside WSL:

```bash
cd windows/clip-listener
GOOS=windows GOARCH=amd64 go build \
    -trimpath -ldflags="-s -w" \
    -o clip-listener.exe .
```

You should end up with a ~2 MB `clip-listener.exe`.

### 3. Install both pieces

```bash
mkdir -p ~/.local/bin ~/.local/share/wsl-clip-bridge
install -m 0755 windows/clip-listener/clip-listener.exe \
    ~/.local/share/wsl-clip-bridge/clip-listener.exe
install -m 0755 wsl/wsl-clip-bridge \
    ~/.local/bin/wsl-clip-bridge
```

Make sure `~/.local/bin` is on your `PATH`. (Most distros do this by
default if the directory exists.)

### 4. Autostart on every WSL shell

Append this snippet to `~/.bashrc`:

```bash
# wsl-clip-bridge autostart
if [ -n "$WAYLAND_DISPLAY" ] && command -v wsl-clip-bridge >/dev/null 2>&1; then
    if ! wsl-clip-bridge --status 2>/dev/null | grep -q running; then
        nohup wsl-clip-bridge >/dev/null 2>&1 &
        disown
    fi
fi
```

The bridge will then start in the background the first time you open
a WSL shell. Subsequent shells see the bridge is already running and do
nothing.

### 5. Add the Claude Code keybinding

Create `~/.claude/keybindings.json` (or add the binding to your existing
one):

```json
{
  "$schema": "https://www.schemastore.org/claude-code-keybindings.json",
  "bindings": [
    { "context": "Chat", "bindings": { "alt+v": "chat:imagePaste" } }
  ]
}
```

This binds `Alt+V` to Claude Code's image-paste handler. Windows Terminal
doesn't grab `Alt+V`, so the keystroke reaches Claude Code.

### 6. Verify

Open a fresh WSL shell. Then:

```bash
# In Windows, copy an image (Win+Shift+S, snip a region).

# Back in WSL:
wl-paste -l         # should include "image/png"
wsl-clip-bridge --status   # should say "running"

# In Claude Code: press Alt+V. The image attaches.
```

## What lands where

| Path | Purpose |
|---|---|
| `~/.local/share/wsl-clip-bridge/clip-listener.exe` | Go-built Win32 listener (~2 MB) |
| `~/.local/bin/wsl-clip-bridge` | Bash daemon: pipes listener → `wl-copy` |
| `~/.bashrc` (snippet) | Autostart on first shell after WSL boot |
| `~/.claude/keybindings.json` | `alt+v` → `chat:imagePaste` |
| `~/.cache/wsl-clip-bridge/` | Saved PNGs + `bridge.log` |

## How it works

```
Windows side                                  WSL side
────────────                                  ──────────
Win+Shift+S
   │   sets BMP on Windows clipboard
   ▼
WM_CLIPBOARDUPDATE
   │
clip-listener.exe (Go, event-driven, ~3-8 MB resident)
   • AddClipboardFormatListener on a message-only window
   • blocks in GetMessage
   • on each event: HBITMAP → GdiPlus encode → PNG file
   • SHA-256 + 3s dedup window kills the WSLg round-trip
   │
   │  stdout: "IMAGE /path/clip-N.png"
   └───── pipe ────────►  wsl-clip-bridge (bash)
                              • wl-copy --type image/png
                              • sleep 0.5s, then re-assert once
                                if WSLg has overwritten our PNG
                              │
                              ▼
                          Wayland clipboard = image/png
                                                │ Alt+V
                                                ▼
                                          Claude Code
                                          chat:imagePaste
                                          → wl-paste image/png
                                          → image attached
```

Event-driven end-to-end except for the single re-assertion check after
each emit, which exists to recover from WSLg's silent Windows→Wayland
clobber.

## Useful commands

```bash
wsl-clip-bridge             # run in foreground (^C stops the bash side)
wsl-clip-bridge --status    # is clip-listener.exe alive?
wsl-clip-bridge --stop      # kill it via taskkill.exe
tail -f ~/.cache/wsl-clip-bridge/bridge.log
```

## Retiring this

If a future Claude Code release reads `image/bmp` correctly **and**
Windows Terminal stops eating Ctrl+V (or you unbind it manually in
Terminal Settings → Actions), the whole bridge becomes redundant —
not incorrect, just unnecessary. Removal is:

```bash
wsl-clip-bridge --stop
rm -rf ~/.local/share/wsl-clip-bridge ~/.local/bin/wsl-clip-bridge
# trim the autostart snippet from ~/.bashrc
# and the Chat-context alt+v entry from ~/.claude/keybindings.json
```

Nothing in the rest of your environment depends on this.
