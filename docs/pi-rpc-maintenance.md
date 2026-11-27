# Explicit offline native-worker maintenance

Maintenance is **off by default**, per worker, and never run by the ticker,
`resolve`, `sweep`, startup or shutdown. It does not stop a worker or change a
bead's state. Development/installation does not authorize live cleanup.

```sh
# Both commands preview by default. Select one closed, stopped native worker.
quartermaster rpc retain <bead> --max-bytes 8388608
quartermaster rpc cleanup <bead>

# Only after reviewing the preview and obtaining lifecycle authorization:
quartermaster rpc retain <bead> --max-bytes 8388608 --yes
quartermaster rpc cleanup <bead> --yes
```

`--yes` applies the operation under the existing class-3 `resolve` autonomy
gate. It is not a force flag. `--by TJ` is only for a decision actually made by
the operator. There is no override for active beads, live leases, missing or
ambiguous owner/session evidence, dirty or unmerged work, or adopted/TUI paths.
A future exceptional transition needs a separately reviewed recovery policy.

Optionally configure a byte budget for explicit `rpc retain` calls:

```toml
[rpc_retention]
event_bytes = 8388608
```

Zero/omitted disables retention; a positive budget must be at least 1024 bytes.
`--max-bytes` overrides the configured budget for that invocation only. This
setting does **not** schedule work or cap a running worker's disk use.

## Preconditions and protected evidence

Both operations acquire the **existing** native session lease, nonblocking,
and hold it until the operation finishes. The lease inode is never replaced
or removed. A supervisor/resume cannot acquire it concurrently. A dry run also
checks the lease, but creates no files and does not claim/release the bead.

Maintenance requires:

- A native-dispatch or completed-fork reservation with the exact private
  `rpc/<bead>/worktree` path and task branch. Adopted, fresh-handoff,
  incomplete-fork, and worktree-free role reservations are deliberately
  excluded, even when a source pane is gone. No Herdr operation is invoked.
- Saved healthy `stopped` state (empty `error`), no child PID, no live saved
  supervisor PID, and an existing native v3 session whose header matches the
  saved session ID and worktree (and fork metadata when forked). `failed`,
  `stopping`, errored, missing/corrupt status, or a disconnected socket alone
  do not qualify.
- A closed bead and complete accepted/rejected request receipts. Ambiguous
  intents, malformed/missing receipt storage and unknown artifacts refuse
  maintenance; reconciliation is never replaced with replay.
- Canonical, regular private artifacts, not symlinks or hard links.

Never deleted or rewritten: `worker.json`, `status.json`, `lease`, `parked`,
`requests/`, `sessions/`, external/TUI sessions, inbox archives, or coordinator
acknowledgment evidence. Diagnostics (`stderr.log`, `supervisor.log`) also stay.
The native reservation continues to prevent implicit TUI redispatch.
These are managed-worker safeguards, not a sandbox against arbitrary same-user
processes writing files or manually opening sessions concurrently. Before apply,
stop any independent editor/build/descendant process using this checkout. A
saved supervisor PID reused by another process also conservatively refuses
maintenance; investigate rather than overriding the lease/status checks.

## Event retention and replay gaps

Retention applies only to `events.jsonl`. It keeps **every response, extension
dialog and extension error**, including response types unknown to this version.
Within the remaining budget it keeps the newest whole other records. If the
protected records plus the gap checkpoint cannot fit, the command refuses
without changing anything; increase the budget rather than discard evidence.

The replacement starts with a `quartermaster_retention_gap` JSON record with
cumulative removed-record/byte counts. Retained protected records can precede
the recent tail: a gap does not imply that every old record was removed.
Incomplete/malformed/oversized logs refuse retention for investigation.

The normalized viewer/feed renders a gap even for a first-time reader. Existing
cursors detect the new file identity and explicitly reset replay. Raw `events`
includes the checkpoint, and `output` prints a GAP notice. Saved `history` and
usage remain unchanged. Retention is not pi context compaction and cannot
summarize or erase a saved conversation.

The implementation scans in bounded memory (one protocol record), writes a
private replacement in the same directory, fsyncs it with its gap checkpoint,
renames it atomically, then fsyncs the directory. A crash before rename leaves
the old log and possibly an unused `.retention-*` stage; after rename the new
log contains its own checkpoint. Re-running computes from the current log and
never interprets abandoned stages as authoritative. Stages are preserved for
inspection, not automatically swept. Temporary free space for the replacement
is required. Repeat retention on a log already within budget is a no-op.

This is a bound on the **successfully retained event file**, not a bound on
total worker storage. Sessions, receipts, diagnostics and protected evidence
are intentionally not subject to automatic expiry.

## Worktree removal and interrupted cleanup

Cleanup additionally checks the configured repository, linked-checkout
registration, branch, unlocked state, and clean status including **ignored and
untracked files** and submodules. The exact head must either be an ancestor of
the configured base or match a merged PR whose merge commit is in that base.
A merged PR with the same branch name but a different head is insufficient.
Cleanup does not fetch or mutate base refs: stale evidence requires a separate
fetch/review before retrying. Git/GitHub lookup errors fail closed.

After verification it durably writes `cleanup.json` with the selected head,
repository and path, rechecks the closed bead, worktree status (including
ignored files), head and branch, then calls Git's non-force `worktree remove`
on that one path and saves completion. Git status uses `--no-optional-locks`
so previews do not refresh the index. **The branch is never deleted.** Sessions
and receipts remain viewable after removal. Git's per-worktree administration,
including its HEAD reflog and worktree-local refs, is removed by Git; this is
not a full archive of detached experiments. Preserve those separately first.

The final status recheck catches files created during a slow merge lookup, but
cannot lock out an unrelated same-user writer during Git removal. In
particular Git itself does not protect ignored files. Do not apply while an
independent writer is using the checkout.

An intent or completed cleanup checkpoint fences `rpc resume`, direct
supervisor startup, and new `rpc fork` preparation from that worker, including
when the checkout is later recreated. No empty session or replacement checkout
is started automatically.

After an interruption, inspect the checkpoint and Git registration first.
Re-running the same cleanup revalidates a still-present checkout; new dirty
files or a changed head refuse removal. If the checkout and its registration
are both gone, the retry records completion without another deletion. If only
the directory is gone but its registration remains, it refuses: it never runs
repository-wide `worktree prune` or guesses away inconsistent metadata.

For a separately authorized rollback, retain the checkpoint as evidence,
restore the recorded branch at the exact recorded path with normal `git
worktree add <recorded-path> <recorded-branch>`, verify `git -C <recorded-path>
rev-parse HEAD` equals the checkpoint's `head`, and verify the unchanged saved
session before explicitly archiving the checkpoint outside the active
`cleanup.json` name (for example `cleanup.restored.json`). Reopen the
bead separately if appropriate, then authorize `rpc resume`. Do not remove the
lease or reservation. There is deliberately no force-restore or automatic
resume path.

## Isolated tests

```sh
GOMAXPROCS=2 go test -p 2 . -run 'TestRPCRetention|TestRPCMaintenance|TestRPCCleanup'
GOMAXPROCS=2 go test -race -p 2 . -run 'TestRPC|TestPi'
```

Fixtures use scratch HOME/XDG/state/config/Beads/session paths, private Git
repositories and stub GitHub/Beads/Herdr executables. No live worker, real task,
model, credentials, installation, service restart or cleanup is used.
