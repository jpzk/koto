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
  `InboxDismiss{ids}`.
- `reserved 4`: kept for proposals. Nothing implements them yet.

Origin fields (group, session, kind) always come from daemon state: the
approvals key, the goal record, or the `notifyDeliver` caller. They never
come from agent-written text. The TUI also passes every agent-written string
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

- The tree shows an **`Inbox (N)`** row above `main`. N is the number of
  pending decisions plus undismissed notifications.
- The inbox is seeded and kept live by `WatchInbox`, which reconnects with
  backoff the same way the state stream does.
- Select the row (enter or click) to open the list. The list is newest
  first; each line shows the time, `group/session`, the kind and a one-line
  subject.

Keys in the list:

- `j`/`k` or the arrow keys: move. `g`/`G`: first/last item.
- `enter` or a mouse click: open the selected item.
  - Tool approval or ask: opens the existing approval card or ask dialog
    (options plus a message box). Submitting calls `Approve` with
    option+msg.
  - Goal plan: opens a small plan view (plan text and criteria) where
    `a`/`d` (or tab then enter) choose approve or deny. A message box
    follows, then enter sends `GoalDecide`. A deny needs a reason.
- `d`: dismiss the selected notification (`InboxDismiss`).
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
