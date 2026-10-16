#!/bin/sh
# Records docs/images/quartermaster-demo.mp4 and .gif: quartermaster running in
# a scratch Herdr session on made-up data.
#
# Everything lives in its own HOME ($QM_DEMO_HOME, default ~/.cache/qmd): a
# fictional repo (acme/notes) with its beads, a Herdr server and its config,
# quartermaster's config, state and ticker. The agents are scripts/demo/agent,
# a stand-in installed as claude and codex that answers from a script, and gh
# is scripts/demo/gh, which answers from a JSON file. No model, no GitHub, no
# real data. Your own Herdr session, beads and config are not touched.
#
# A tmux server with no config hosts the Herdr client; each frame is a
# `tmux capture-pane -e` dump. scripts/demo/frames turns them into SVGs, leaving
# out the waits marked here (an agent starting, the ticker's next GitHub
# pass), rsvg-convert makes PNGs and ffmpeg the video.
#
# Needs: go, herdr, bd, tmux, python3, rsvg-convert, ffmpeg, ImageMagick. Takes ~2 minutes.
set -eu
cd "$(dirname "$0")/../.."
SRC=$(pwd)
# Who and where you really are, for the check that none of it was recorded.
REAL_HOME=$HOME
REAL_USER=$(id -un)
REAL_HOST=$(hostname 2>/dev/null || cat /etc/hostname 2>/dev/null || echo localhost)
# Short: Herdr's socket lives under it and a socket path must fit in 108 bytes.
D=${QM_DEMO_HOME:-$HOME/.cache/qmd}
W=132
H=36
TMUX_CMD="tmux -L qmdemo -f $D/tmux.conf"

say() { printf 'record: %s\n' "$*" >&2; }

teardown() {
  if [ -f "$D/env.sh" ]; then
    (. "$D/env.sh" && quartermaster ticker stop >/dev/null 2>&1; herdr server stop >/dev/null 2>&1) || true
  fi
  tmux -L qmdemo kill-server 2>/dev/null || true
}

# --- the scratch world ------------------------------------------------------

teardown
rm -f "$D/recording"
sleep 0.5
rm -rf "$D"
# On any failure, stop the capture loop and the scratch session.
trap 'rm -f "$D/recording"; teardown' EXIT
mkdir -p "$D/bin" "$D/plugin/bin" "$D/home/.config/herdr" "$D/home/.config/quartermaster" "$D/rec"
H_=$D/home

say "building quartermaster"
go build -buildvcs=false -o "$D/plugin/bin/quartermaster" .
ln -s quartermaster "$D/plugin/bin/qm"
cat >"$D/plugin/herdr-plugin.toml" <<'EOF'
id = "quartermaster"
name = "Quartermaster"
version = "0.0.0-demo"
min_herdr_version = "0.9.1"
description = "Demo build"
platforms = ["linux", "macos"]

[[actions]]
id = "board"
title = "Quartermaster: board"
contexts = ["global", "workspace"]
command = ["bin/quartermaster", "action", "board"]

[[panes]]
id = "board"
title = "Quartermaster"
placement = "popup"
width = "85%"
height = "80%"
command = ["bin/quartermaster", "board"]
EOF

cp scripts/demo/agent "$D/bin/claude"
cp scripts/demo/agent "$D/bin/codex"
cp scripts/demo/gh "$D/bin/gh"
cat >"$D/bin/hcall" <<'EOF'
#!/usr/bin/env python3
# hcall METHOD [JSON]: one request on the Herdr socket.
import json, os, socket, sys
path = os.environ.get("HERDR_SOCKET_PATH") or os.path.join(os.environ["XDG_CONFIG_HOME"], "herdr", "herdr.sock")
s = socket.socket(socket.AF_UNIX); s.connect(path)
s.sendall((json.dumps({"id": "demo", "method": sys.argv[1], "params": json.loads(sys.argv[2]) if len(sys.argv) > 2 else {}}) + "\n").encode())
print(s.makefile().readline().strip())
EOF
ln -s ../plugin/bin/quartermaster "$D/bin/quartermaster"
ln -s ../plugin/bin/quartermaster "$D/bin/qm"
chmod +x "$D/bin/"*

cat >"$D/env.sh" <<EOF
unset HERDR_PANE_ID HERDR_ENV HERDR_TAB_ID HERDR_SOCKET_PATH HERDR_WORKSPACE_ID HERDR_BIN_PATH BEADS_DIR BEADS_ACTOR TMUX
for v in \$(env | sed -n 's/^\(QUARTERMASTER_[A-Z_]*\|FLATCIRCLE_[A-Z_]*\|KELPIE_[A-Z_]*\|SHEPHERD_[A-Z_]*\)=.*/\1/p'); do unset \$v; done
export HOME=$H_ XDG_CONFIG_HOME=$H_/.config XDG_STATE_HOME=$H_/.local/state XDG_DATA_HOME=$H_/.local/share XDG_CACHE_HOME=$H_/.cache
export BEADS_DIR=$H_/src/notes/.beads QM_DEMO=$D PATH=$D/bin:\$PATH SHELL=/bin/zsh COLORTERM=truecolor
export GIT_AUTHOR_NAME=Demo GIT_AUTHOR_EMAIL=demo@example.com GIT_COMMITTER_NAME=Demo GIT_COMMITTER_EMAIL=demo@example.com
EOF
. "$D/env.sh"

cat >"$HOME/.zshrc" <<'EOF'
PROMPT='%F{blue}%1~%f %F{green}❯%f '
EOF
git config --global init.defaultBranch main
git config --global user.name Demo
git config --global user.email demo@example.com

cat >"$XDG_CONFIG_HOME/herdr/config.toml" <<'EOF'
onboarding = false

[theme]
name = "tokyo-night"

[ui]
sidebar_width = 24

[ui.sound]
enabled = false

[ui.sidebar.agents]
rows = [["state_icon", "agent"], [{ token = "$sh_state", rules = [{ starts_with = "needs you", fg = "#f7768e", bold = true }, { starts_with = "checks failing", fg = "#f7768e" }, { starts_with = "review", fg = "#e0af68" }, { starts_with = "ready to merge", fg = "#9ece6a", bold = true }, { starts_with = "merged", fg = "#9ece6a" }] }]]

[ui.sidebar.spaces]
rows = [["state_icon", "workspace"]]

[worktrees]
directory = "~/worktrees"

[[keys.command]]
key = "prefix+j"
type = "plugin_action"
command = "quartermaster.board"
description = "Quartermaster board"
EOF

cat >"$XDG_CONFIG_HOME/quartermaster/config.toml" <<EOF
repo = "$HOME/src/notes"
branch_prefix = "demo/"
tick_seconds = 5
gh_seconds = 30
idle_seconds = 2
linear_prefixes = ["APP"]
verify_minutes = 0
EOF

say "seeding the notes repo and its beads"
mkdir -p "$HOME/src/notes/internal/search"
cd "$HOME/src/notes"
git init -q
printf '# notes\n\nA small notes app.\n' >README.md
printf 'package search\n' >internal/search/handler.go
git add -A && git commit -qm "Initial commit"
git remote add origin https://github.com/acme/notes.git
bd init --prefix notes --stealth --quiet >/dev/null 2>&1
bd config set status.custom needs_me >/dev/null
bd metrics off >/dev/null 2>&1 || true
bead() { # id priority type title [labels]
  bd create --id "$1" -p "$2" -t "$3" "$4" ${5:+--labels "$5"} --silent >/dev/null
}
bead notes-7qk 1 task "Add rate limiting to the API"
bead notes-9wc 2 task "Upgrade the Postgres driver to v5"
bead notes-h6p 2 task "Dark mode for the settings page"
bead notes-r8t 2 task "Write the v2 migration guide"
bead notes-c4j 2 task "Paginate the search endpoint" next
bead notes-d1f 1 feature "CSV export for reports"
bead notes-w8m 2 feature "Webhooks for new comments"
bead notes-s6y 3 task "Document the release process"
cd "$SRC"

pr() { # number bead-branch title state review merge-state checks-conclusion
  reviews="[]"
  # An approval counts only for the head it reviewed, and only while fresh.
  [ "$5" = APPROVED ] && reviews="[{\"author\": {\"login\": \"a-reviewer\"}, \"state\": \"APPROVED\", \"submittedAt\": \"$(date -u +%Y-%m-%dT%H:%M:%SZ)\", \"commit\": {\"oid\": \"c0ffee$1\"}}]"
  printf '{"number": %s, "title": "%s", "url": "https://github.com/acme/notes/pull/%s", "headRefName": "%s", "state": "%s", "isDraft": false, "mergedAt": null, "reviewDecision": "%s", "mergeStateStatus": "%s", "headRefOid": "c0ffee%s", "reviews": %s, "autoMergeRequest": null, "statusCheckRollup": [{"name": "test", "status": "COMPLETED", "conclusion": "SUCCESS"}, {"name": "lint", "status": "COMPLETED", "conclusion": "SUCCESS"}, {"name": "e2e", "status": "COMPLETED", "conclusion": "%s"}]}' \
    "$1" "$3" "$1" "$2" "$4" "$5" "$6" "$1" "$reviews" "$7"
}
world() { # #45's review decision and merge state, #48's e2e conclusion
  {
    printf '{"login": "demo", "prs": ['
    pr 45 demo/notes-9wc-postgres "APP-11: Upgrade the Postgres driver to v5" OPEN "$1" "$2" SUCCESS
    printf ', '
    pr 47 demo/notes-r8t-guide "Write the v2 migration guide" OPEN REVIEW_REQUIRED BLOCKED SUCCESS
    printf ', '
    pr 48 demo/notes-h6p-dark-mode "APP-17: Dark mode for the settings page" OPEN "" "$3" "$4"
    printf ']}\n'
  } >"$D/world.tmp" && mv "$D/world.tmp" "$D/world.json"
}
world REVIEW_REQUIRED BLOCKED CLEAN SUCCESS

# --- Herdr --------------------------------------------------------------------

say "starting a scratch herdr"
printf 'set -g status off\nset -g default-terminal tmux-256color\nset -g escape-time 0\n' >"$D/tmux.conf"
$TMUX_CMD new-session -d -s demo -c "$HOME" -x $W -y $H "sh -c '. $D/env.sh; cd; exec herdr'"
i=0
until hcall pane.list >/dev/null 2>&1; do
  i=$((i + 1)); [ $i -lt 100 ] || { say "herdr did not start"; exit 1; }
  sleep 0.1
done
hcall plugin.link "{\"path\": \"$D/plugin\"}" >/dev/null
quartermaster configure >/dev/null

jsonget() { python3 -c "import json, sys; d = json.load(sys.stdin)
for k in sys.argv[1:]: d = d[k] if not k.isdigit() else d[int(k)]
print(d)" "$@"; }
inbox_count() { ls "$XDG_STATE_HOME/quartermaster/inbox/"*.json 2>/dev/null | wc -l; }

say "starting workers on four beads (not recorded)"
quartermaster dispatch notes-7qk >/dev/null
quartermaster dispatch notes-9wc >/dev/null
quartermaster dispatch notes-h6p --agent codex >/dev/null
quartermaster dispatch notes-r8t >/dev/null
bd note notes-9wc "PR: https://github.com/acme/notes/pull/45" >/dev/null
bd note notes-r8t "PR: https://github.com/acme/notes/pull/47" >/dev/null
bd note notes-h6p "PR: https://github.com/acme/notes/pull/48" >/dev/null
quartermaster ticker stop >/dev/null 2>&1 || true
rm -f "$XDG_STATE_HOME/quartermaster/state.json"
quartermaster ticker start >/dev/null
i=0
until grep -q '"c0ffee45"' "$XDG_STATE_HOME/quartermaster/state.json" 2>/dev/null; do
  i=$((i + 1)); [ $i -lt 300 ] || { say "the ticker never saw the PRs"; exit 1; }
  sleep 0.2
done

say "the coordinator, and two things for the inbox (not recorded)"
for w in $(hcall workspace.list | python3 -c 'import json, sys
for w in json.load(sys.stdin)["result"]["workspaces"]:
    if w["label"] == "~": print(w["workspace_id"])'); do
  hcall workspace.close "{\"workspace_id\": \"$w\"}" >/dev/null
done
quartermaster coordinator >/dev/null
# A worker asks a question, and a PR's e2e check fails: the ticker turns both
# into inbox items on its next passes.
bd note notes-7qk "Question: limit per API key or per client IP? (a) per key, 100 req/min (b) per IP, 60 req/min (c) both" >/dev/null
bd update notes-7qk --status needs_me >/dev/null
world REVIEW_REQUIRED BLOCKED BLOCKED FAILURE
i=0
until [ "$(inbox_count)" -ge 2 ]; do
  i=$((i + 1)); [ $i -lt 450 ] || { say "the inbox never filled"; exit 1; }
  sleep 0.2
done
# Let the ticker's nudge about them land before the coordinator's screen is wiped.
sleep 9

# The coordinator's workspace: its pane, and a worker's screen beside it.
COORD_PANE=$(herdr agent get quartermaster | jsonget result agent pane_id)
MIRROR=$(herdr pane split "$COORD_PANE" --direction right --ratio 0.6 | jsonget result pane pane_id)
hcall pane.send_text "{\"pane_id\": \"$MIRROR\", \"text\": \"clear; herdr agent attach notes-9wc\\r\"}" >/dev/null
herdr agent prompt quartermaster "/clear" >/dev/null
hcall pane.focus "{\"pane_id\": \"$COORD_PANE\"}" >/dev/null
sleep 2

# --- the recording ------------------------------------------------------------

T0=$(date +%s%3N)
now() { echo $(($(date +%s%3N) - T0)); }
mark() { echo "$(now) $1" >>"$D/rec/marks.txt"; }
screen() { $TMUX_CMD capture-pane -p -t demo; }
wait_for() { # pattern [timeout seconds]
  n=$((${2:-30} * 10))
  until screen | grep -q -- "$1"; do
    n=$((n - 1)); [ $n -gt 0 ] || { say "timed out waiting for: $1"; return 1; }
    sleep 0.1
  done
}
type_text() {
  printf '%s\n' "$1" | fold -w1 | while IFS= read -r c; do
    $TMUX_CMD send-keys -t demo -l -- "${c:- }"
    sleep 0.07
  done
}
key() { $TMUX_CMD send-keys -t demo "$@"; }

touch "$D/recording"
(
  while [ -f "$D/recording" ]; do
    $TMUX_CMD capture-pane -e -p -t demo >"$D/rec/$(now).ans" 2>/dev/null || true
    sleep 0.08
  done
) &
CAPTURE=$!

say "recording"
sleep 1.5
# You ask; the coordinator reads quartermaster context and sums it up.
type_text "what's going on?"
sleep 0.4
key Enter
wait_for "Start it?" 30
sleep 2.8
type_text "go"
sleep 0.4
key Enter
sleep 0.3
mark cut-start
wait_for "Bash(quartermaster dispatch" 30
mark cut-end
wait_for "tell you when it needs you" 15
# The new worker's screen joins the coordinator's workspace.
NEW=$(herdr pane split "$MIRROR" --direction down --ratio 0.45 | jsonget result pane pane_id)
hcall pane.send_text "{\"pane_id\": \"$NEW\", \"text\": \"clear; herdr agent attach notes-c4j\\r\"}" >/dev/null
hcall pane.focus "{\"pane_id\": \"$COORD_PANE\"}" >/dev/null
mark cut-start
wait_for "worker · notes-c4j" 15
mark cut-end
wait_for "Tests pass" 20
sleep 1.2
mark still
sleep 1.3
# The board, over it all: filter to what's in review, open one.
key C-b j
wait_for "┌Quartermaster" 10
sleep 1.4
key /
sleep 0.3
type_text "review"
sleep 0.9
key Enter
sleep 0.4
key Space
wait_for "Latest note" 10
until ! screen | grep -q "loading…"; do sleep 0.1; done
sleep 2.4
key Escape
sleep 0.3
key q
sleep 1.0
# PR #45 is approved; the ticker's next GitHub pass sees it and nudges the
# coordinator.
world APPROVED CLEAN BLOCKED FAILURE
nudges=$(screen | grep -c "quartermaster ticker")
mark cut-start
wait_for "ready to merge" 90
mark cut-end
sleep 1.5
mark cut-start
until [ "$(screen | grep -c "quartermaster ticker")" -gt "$nudges" ]; do sleep 0.1; done
mark cut-end
wait_for "Merge it?" 30
sleep 3

rm -f "$D/recording"
wait $CAPTURE 2>/dev/null || true
teardown

# --- frames, video and a check for real data ---------------------------------

say "checking every frame for real data"
# Your home, user and host name, any email address, and whatever
# QM_DEMO_DENY adds (an extended regex: your org, ticket keys, ...).
deny="$REAL_HOME|$REAL_USER|$REAL_HOST|[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+\\.[A-Za-z]{2,}${QM_DEMO_DENY:+|$QM_DEMO_DENY}"
if cat "$D/rec/"*.ans | sed 's/\x1b\[[0-9;:?]*[A-Za-z]//g' | grep -iE -- "$deny" | head -5 | grep .; then
  say "real data in the frames; not writing the video"
  exit 1
fi

say "rendering"
go run -buildvcs=false ./scripts/demo/frames -in "$D/rec" -out "$D/frames" -w $W -h $H -title "herdr"
ls "$D/frames/"*.svg | xargs -P 8 -I{} sh -c 'rsvg-convert --zoom 2 --output "${1%.svg}.png" "$1"' _ {}
cd "$D/frames"
ffmpeg -loglevel error -y -f concat -safe 0 -i concat.txt \
  -vf "fps=30,format=yuv420p" -c:v libx264 -crf 22 -preset slow -movflags +faststart "$SRC/docs/images/quartermaster-demo.mp4"
# The GIF keeps each distinct frame with its own delay, at half the PNGs'
# size, and stores only what changed from the frame before.
ffmpeg -loglevel error -y -f concat -safe 0 -i concat.txt -fps_mode vfr \
  -vf "scale=iw/2:-1:flags=lanczos,split[a][b];[a]palettegen=max_colors=128[p];[b][p]paletteuse=dither=none" \
  "$D/full.gif"
magick "$D/full.gif" -layers Optimize "$SRC/docs/images/quartermaster-demo.gif"
cd "$SRC"
rsvg-convert --zoom 2 --output "$SRC/docs/images/quartermaster.png" "$D/frames/still.svg"
cp "$D/frames/still.svg" "$SRC/docs/images/quartermaster.svg"
ls -l docs/images/quartermaster*
