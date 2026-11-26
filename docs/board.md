# Quartermaster Board TUI (Mobile & Desktop)

`quartermaster board` is a pure-Go terminal UI designed to work smoothly both
over SSH on mobile terminals (such as Moshi on narrow portrait/landscape
screens) and on wide desktop terminals (such as Ghostty or Rex inside Herdr).

The workflow is built so routine decisions never require typing into a
coordinator prompt: open the **Needs You Decision Queue** (`Q`, or launch
directly with `quartermaster board --queue`), triage approvals, recommendations,
`RUN:` commands, and snoozes straight in the TUI, and reserve `@` / the
coordinator prompt for genuinely new or ad-hoc requests.

## Launching

```sh
# Full board (all open beads grouped by state)
quartermaster board

# Decision Queue directly (only items waiting on your input)
quartermaster board --queue
```

## Decision Queue (`Q` / `--queue`)

The Decision Queue filters to items waiting on human input and sorts them by
urgency:

1. Live worker approval prompts (`PROMPT`)
2. `CRITICAL:` `needs_me` questions (`CRITICAL`)
3. Structured `QUESTION:` / `OPTIONS:` decisions (`DECIDE`, showing `C<n>` and `★rec<N>`)
4. Manual `RUN:` commands (`RUN`)
5. Free-text `needs_me` questions (`QUESTION`)

Snoozed non-prompt, non-`CRITICAL:` items are hidden by default (`H` toggles
them back into view).

### Zero-friction triage actions

- **Direct `1`–`9` option picking everywhere:** press `1`–`9` (or tap/click an
  option in the wide split preview or Answer view) directly from the Decision
  Queue, main board (on structured `needs_me` beads), or Answer view. The chosen
  option highlights inline (`❯`) with an inline confirmation banner — press `y`
  or `↵` to send, `e` to attach a note, another digit `1`–`9` to switch options,
  or `n`/`esc` to cancel. Press `t` to enter a custom free-text answer.
- **Immediate optimistic answer feedback:** submitting an option (`1`–`9`),
  custom free-text answer (`t`), approval prompt choice, `RUN:` completion, or
  direct worker reply immediately marks the row, detail/preview pane, and Answer
  view as in-flight (`✓ sending:` / `✓ Submitting:`), blocks accidental
  duplicate submissions while the write and reload are pending, transitions to
  `✓ answered:` once recorded, and rolls back cleanly with an error status if
  the write fails.
- **Section-stable cursor and scroll position:** when answering, snoozing,
  closing, or changing status moves the selected item out of its section or
  filtered view, the cursor stays at your place in that section (next row,
  previous row if last, or nearest selectable row if the section emptied) and
  preserves the list scroll offset instead of following the moved item to the
  bottom or jumping back to index 0. Moving to another row while a previous
  answer is still in flight keeps focus on your new row when the async reload
  completes.
- **Accept recommendation in one confirm (`f` in queue/list, or `f` / `↵` inside Answer view):**
  confirms the worker's stated `★` recommendation (`y/n · ↵ send · e +note`).
- **Scannable Open Question, Done, and History panes:** wide split and detail
  views separate the crisp Question, highlighted `★ Recommendation`, and
  numbered Options above longer `Details`/`Evidence` prose, surface completed
  milestones under `Done` (`✓`), and cap verbose `History` notes with hanging
  indents so progress is scannable at a glance (`b` opens full `bd show`).
- **Auto-advance through the queue:** opening any item from the Decision Queue
  (`↵` / `A`) displays its queue position (`[1/N]`), lets you step with `]`/`[`,
  and automatically advances to the next waiting item after you answer, complete
  a `RUN:`, or snooze.
- **Native `RUN:` cards (`A` or `↵` on a `RUN:` item):** displays the exact
  command and reason; press `c` to copy to your clipboard (via OSC 52 over SSH
  plus local Wayland clipboard when available), and `d` / `↵` once run to record
  `RAN: <cmd> → <result>`, return the bead to `in_progress`, and prompt the idle
  worker.
- **Snooze (`z`):** available from the main board, Decision Queue, and Answer
  view (`1 2h · 2 4h · 3 1d · 4 3d · 5 1w · 0 clear · t custom`).

## Responsive Layouts (`L`)

`quartermaster board` automatically adapts on startup and instantaneously on
terminal resize (`SIGWINCH`):

| Terminal width | Auto layout | Behavior |
|---|---|---|
| `< 80` cols (mobile / Moshi) | `compact` | 2-line touch-friendly cards keeping bead ID, priority, badges (`★rec`, `C<n>`, `PROMPT`, `RUN`), and title readable down to ~40 cols, with concise mobile hints. |
| `80–129` cols | `standard` | Classic single-line table with optional stacked detail pane (`space`/`v`). |
| `>= 130` cols (desktop / Ghostty / Rex) | `wide` | Live side-by-side split pane: list or Decision Queue on the left, live decision preview + detail pane on the right (`J`/`K` scrolls right pane). |

Press `L` at any time to cycle `auto → compact → standard → wide`.

## Mouse, Touch, Clipboard, and SSH

- **SGR 1006 mouse & touch:** tap/click rows to select, tap a selected row to
  act/open Answer or Detail, tap numbered options directly inside Answer view,
  tap `y` / `n` / `e` on confirmation bars, tap row 0 to toggle the Decision
  Queue, and use scroll wheel / two-finger scroll. Mouse tracking is cleanly
  restored on exit and during pager handoff (`b` / `V`).
- **Instant redraws:** both `SIGWINCH` resizes and background data/action
  completions wake the event loop immediately over an internal self-pipe.
- **OSC 52 clipboard & SSH links:** `y` (copy bead ID), `p`/`c` (copy `RUN:`
  command), and `o`/`l` (open PR/Linear over SSH or headless sessions) emit
  OSC 52 clipboard sequences so copies land directly on your phone or laptop
  clipboard over SSH.
