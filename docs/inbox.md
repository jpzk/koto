# Inbox

The inbox is one list of everything that waits on the operator: decisions
(tool approvals, `cs-ask` questions, goal plans) and notifications
(`cs-notify`). It is a daemon-side **projection** of state the daemon already
keeps, plus one new store for notifications. There is no second decision
path: answering an inbox item calls the same RPCs as answering it anywhere
else.

Source: `daemon/inbox.go`, `tui/inbox.go`, `protocol/koto.proto`
(`rpc Inbox`, `WatchInbox`, `GoalDecide`, `InboxDismiss`).

## Kinds

`InboxItem` is a oneof:

- `approval = 1` (`PendingApproval`): every open entry of the approvals
  table. That covers tool approvals from `confirm` sessions and goal or agent
  questions raised with `cs-ask` (kind `ask`, optionally with options).
  Answer with the existing `Approve{allow, msg, option}` RPC. `option` is the
  1-based ask option; 0 means none, and a `msg` is still allowed.
- `goal_plan = 2` (`GoalItem`): every goal in status `awaiting_approval`.
  Answer with `GoalDecide{group, name, allow, msg}`.
  - allow: the same code path as `GoalApprove`. A non-empty `msg` is also
    recorded as a steer.
  - deny: the goal goes back to planning, and `msg` is the re-plan reason.
- `notification = 3` (`InboxNotification`): every notification that passes
  through `notifyDeliver`, which is the single funnel for `cs-notify` and
  daemon notices, at both `normal` and `high` severity. Remove one with
  `InboxDismiss{ids}`. Field `source = 8` (additive) records who raised it:
  `guest` (an agent, through `cs-notify` / ctl `notify`) or `daemon` (a
  daemon notice). Anything else is dropped to empty, and entries written
  by an older daemon have it empty too.
- `reserved 4`: kept for proposals. Nothing implements them yet.

Origin fields (group, session, kind) always come from daemon state: the
approvals key, the goal record, or the `notifyDeliver` caller. They never
come from agent-written text.

Attribution of daemon notices: a daemon notification that is *about* one
group (its forwarded log alerts, per-group disk/CPU warnings, posture
alerts, goal notices) is filed under **that group** in the inbox, with
`source = daemon`. Only host-wide notices (host disk, for example) stay
under `main`. The banner and transcript marker still go to `main` as
before; only the inbox entry moves. When the entry is filed under a group
other than the one that delivered it, the session is dropped. The effect on
the ACL: a client scoped to group X now sees daemon notices about X, and a
`main`-only client no longer does.

The TUI also passes every agent-written string
(titles, bodies, questions, plan text, option labels) through `cardText`, so
terminal escape sequences show up as visible text instead of being executed.

## ACL

- `Inbox` and `WatchInbox` need only the verb. The answer is filtered **per
  item** by the caller's grant targets, the same way `List`/`WatchState` are,
  so a caller only sees items from groups it can see.
- `GoalDecide` and `InboxDismiss` are operator-only, like `Approve` and
  `GoalAmend`. An agent identity gets `PermissionDenied`.

## Caps and persistence

- The notification store holds at most **50 undismissed notifications per
  group** and **500 in total**. When a cap is hit, the oldest is evicted.
- It is persisted to `<KOTO_HOME>/inbox.json` and survives a daemon restart.
- Titles and bodies have already been flattened to one line and truncated by
  `notifyDeliver` (the same limits as the notification banner).
- A dismiss applies for every client. There is no per-client read state and
  no resolved history.
- Approvals and goal plans are not stored by the inbox. They leave it as
  soon as they are answered anywhere: in the inbox, in the group's own
  approval card, or through ctl.

## Snapshot model

`WatchInbox` follows the `WatchState` pattern:

- The first frame is the full, ACL-filtered set.
- After that, every change sends a new **full snapshot**.
- There is no sequence number, cursor, ring buffer, resume or pagination. A
  client that reconnects just takes the next full frame.
- `Inbox` is the one-shot version of the same snapshot.

`InboxResp{items, ts}` lists items newest first.

## TUI

- `☰` is the inbox logo. The tree shows a **`☰ Inbox (N)`** row above `main`.
  N is the number of pending decisions plus undismissed notifications.
- The top status bar shows the same counts centered between the group and
  the activity/tok-s side, gray like `idle`: `☰ 2 decide · 5 note`, or the
  short `☰ 2·5` when the bar is tight (the short form still wins over the
  right side, which is clipped). Hidden while nothing waits.
- The inbox is seeded and kept live by `WatchInbox`, which reconnects with
  backoff the same way the state stream does.
- Select the row (enter or click) to open the list. The list is newest
  first. Each row has these columns: time, type, origin and a one-line
  subject.
  - The type column comes right after the time and is fixed width: `NOTE`
    for a notification, `DECIDE` for anything waiting on the operator. A high
    notification shows by color (alert), not by text. A decision's subject
    starts with what it is: `ask: `, `tool: ` or `plan: `.
  - The origin column is `group · session`, or `group · —` when there is no
    session. It is padded to the widest visible origin, capped at
    max(8, width/3) and truncated past that, so the columns line up.
  - When a notification is selected, the detail line starts with
    `from guest · ` or `from daemon · ` (the `source` field), if set.
- Filter: the title row shows `all (N) · decisions (N) · notifications (N)`,
  with the active filter highlighted. `decisions` means every kind except
  notifications (tool approvals, asks, goal plans). The filter stays set
  while the TUI runs, including after you close and reopen the inbox. An
  empty filtered list says `nothing under <filter> — tab for the others`.
- Row colors come only from existing theme colors; no new hex values were
  added.
  - time: gray
  - group: the main accent
  - session: the dimmer accent
  - kind: decisions in yellow, notifications in emerald, `high`
    notifications in rose (underlined in mono mode)
  - subject: the default foreground
  - Column padding has no color. The selected row keeps the plain inverse
    bar.
  - Some themes give the accent, dimmer accent and gray the same color
    (boysenberry, kawaii, marble, soyuz). In those, group and session use
    the next palette color that differs from gray and from each other.
  - With `NO_COLOR` set (or ASCII mode), rows contain no escape sequences.

Keys in the list (they act on the **filtered** list):

- `tab` / `shift+tab`, `→` / `←`, or a click on a label in the title row: next / previous / that filter (all → decisions →
  notifications). `f` also cycles forward. The cursor is clamped when the
  filter changes or items disappear.
- `j`/`k` or the arrow keys: move. `g`/`G`: first/last item.
- `enter` or a mouse click: open the selected item.
  - Tool approval or ask: opens the existing approval card or ask dialog
    (options plus a message box). Submitting calls `Approve` with
    option+msg.
  - Goal plan: opens a small plan view (plan text and criteria) where
    `a`/`d` (or tab then enter) choose approve or deny. A message box
    follows, then enter sends `GoalDecide`. A deny needs a reason.
- `d`: dismiss the selected notification (`InboxDismiss`). It does nothing
  on a decision.
- `D`: dismiss every notification visible under the current filter, in a
  single `InboxDismiss` with all their ids. Decisions are never touched.
- `esc`: go back.
- The list shows an empty state when nothing is pending.

## Example flows

### Ask with options (peer to operator to peer)

```sh
# inside a peer group
cs-ask "which db?" "postgres — HA" "sqlite — smaller ops"
```

1. The daemon adds a `kind=ask` approval, and `WatchInbox` pushes a snapshot
   that includes it.
2. The operator opens it from the inbox and picks option 2. If they add a
   message, the client calls `Approve{group, session, id, allow:true,
   option:2, msg}`.
3. The approval resolves, and the next snapshot no longer contains it.
4. The peer's `cs-ask` prints `sqlite — smaller ops` followed by the
   message.

### Notification

```sh
cs-notify -s high "deploy failed" "see /workspace/notes/deploy.log"
```

The notification shows up as a banner and as an inbox item. Pressing `d` in
the inbox sends `InboxDismiss{ids}`, and the item disappears for every
watcher.

### Goal plan

When a peer's goal is set with `plan` (the default), it stops in
`awaiting_approval`. It appears as a `goal_plan` item. In the plan view:

- `a` then enter (optionally with a message, which is recorded as a steer)
  starts iterating.
- `d` plus a reason sends the goal back to planning, with that reason as the
  re-plan input.

## Testing locally

Unit tests run in the pinned Go image from the Makefile (`GO_IMAGE`):

```sh
IMG=$(sed -n 's/^GO_IMAGE ?= //p' Makefile)
for m in daemon tui; do
  podman run --rm -v "$PWD":/src -w /src/$m -e GOWORK=/src/go.work "$IMG" \
    sh -c 'go vet ./... && go test -count=1 ./...'
done
scripts/e2e-inbox.sh e2e.log       # isolated-daemon end to end, no VM needed
```

`scripts/e2e-inbox.sh` runs `TestInboxE2E` (`KOTO_INBOX_E2E=1`), all inside
the test process:

- A production mTLS gRPC daemon on an ephemeral loopback port.
- The shipped `cs-ask` and `cs-notify` scripts as the peer.
- A gRPC client that watches the inbox, answers the ask with option 2 plus a
  message, and dismisses the notification.

It never touches `:8443` or `/var/lib/koto`.

Manual check against a dev daemon (needs a host that can boot guests):

```sh
make dev                 # shell 1: isolated dev daemon (.dev/, DEV_PORT)
make dev-tui             # shell 2: TUI against it
. .dev/env && koto ctl spawn peer && koto ctl send peer \
  'run: cs-notify "hello" "from peer"; cs-ask "which db?" "postgres" "sqlite"'
```

The TUI's `Inbox (N)` row then shows 2. Open the ask, pick an option and
type a message; the peer's `cs-ask` prints the choice. Press `d` on the
notification to dismiss it.
