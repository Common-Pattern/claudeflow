# claudeflow

claudeflow drives a coding agent from GitHub issue labels. Label an issue, and
it claims the issue, creates a git worktree, brings up an isolated environment
the project defines, runs the agent CLI against the issue, opens a pull
request, gets CI green, and merges the result into an integration branch.

It is a generic extraction of a system that has been running against a real
product repository, not a demonstration. The orchestration — the state machine,
the locking, the slot allocation, the merge rules — is fixed. Everything that
varies between projects is a hook or a config key.

> **Early. The command-line interface and the configuration schema may change
> between versions.** Pin a version if you depend on either.

## Runtime dependencies

| Dependency | Why |
| --- | --- |
| `gh`, authenticated | every GitHub call goes through it |
| `git` | worktrees, branches, merges |
| an agent CLI (`claude` by default) | does the actual work |

claudeflow holds no GitHub token of its own. It shells out to `gh` because `gh`
already holds the operator's credentials, refreshes them when they expire, and
resolves the host — so the same binary works against github.com and an
enterprise host with no additional configuration. The consequence: whatever
`gh auth status` reports is what claudeflow can do, and re-authenticating `gh`
is how you fix a permissions failure.

Check before first run:

```sh
gh auth status
git --version
claude --version
```

## Install

```sh
go install github.com/Common-Pattern/claudeflow/cmd/claudeflow@latest
```

Or download an archive from the releases page and put the binary on `PATH`:

```sh
tar -xzf claudeflow_<version>_linux_amd64.tar.gz
install -m 0755 claudeflow ~/.local/bin/claudeflow
```

Release archives are built for linux and darwin, amd64 and arm64. There is no
windows build: claudeflow manages POSIX process groups and shells out to `sh`.

## The label state machine

One label carries the whole state of an issue. The label swap is the lock —
there is no separate lease file, no database, and no coordination between
processes beyond GitHub itself. A tick that crashes loses nothing, because the
next tick reads the same labels back.

| Label | Meaning | Applied by |
| --- | --- | --- |
| `claude` | queued: work this issue | a human |
| `claude:working` | claimed and running. Swapping `claude` for this label **is** the lock | claudeflow |
| `claude:landed` | merged into the integration branch | claudeflow |
| `claude:question` | the agent asked something and is waiting for an answer | claudeflow |
| `claude:blocked` | the run stopped on something it could not resolve | claudeflow |
| `claude:planning` | alongside `claude`, inverts the run into conversation only | a human |

`claude:planning` produces no branch, no commits and no files. The agent reads
the repository and thinks in the issue: its comments are the only artefact of
the run. Planning runs hold no environment slot and are budgeted separately
from builds, so a conversation does not queue behind a two-hour build.

### A comment re-queues the issue

**A comment from the configured user on an issue in any state re-queues it.**
This is the whole re-entry mechanism. `claude:blocked` and `claude:question`
need no manual re-label: answering the agent's question *is* the re-label.
Closed issues are watched too, so a reply on a closed thread starts a run.

Only the one account named in `user:` counts. The agent comments on the same
threads, and without that filter it would read its own comments as
instructions and answer itself indefinitely.

The same rule covers the standing integration pull request: a comment from the
configured user on it starts a review run, which addresses the comments and
lands the fixes. Review runs are scheduled ahead of new issues — someone is
waiting on those, and wrong work already on the integration branch matters more
than work that has not started.

## Configuration

claudeflow reads one YAML file. Below is the worked case for the SlotBooks
repository; every key not shown has a default.

```yaml
# The repository, in owner/name form. Everything runs against this one repo.
repo: Common-Pattern/slotbooks

# The single account whose comments are instructions. Required, and there is no
# default: without it the agent treats its own comments as instructions.
user: sudhirj

branches:
  # Never touched by the agent. The integration branch eventually merges here,
  # and a human does that merge.
  base: main
  # Where the agent lands work, and what the operator tests. Set this equal to
  # `base` for a project with no staging branch; claudeflow then maintains no
  # standing pull request.
  integration: preview
  # Prepended to every branch the agent creates.
  prefix: claude/

labels:
  queued: claude
  working: claude:working
  landed: claude:landed
  question: claude:question
  blocked: claude:blocked
  planning: claude:planning

limits:
  # How many slot-holding runs may be live at once. Must not exceed the number
  # of slots in the pool.
  maxBuilds: 2
  # Budgeted apart from builds: planning runs hold no slot and finish in
  # minutes.
  maxPlanning: 2
  buildTimeout: 150m
  planTimeout: 30m
  # How long dispatch pauses after the agent CLI reports a usage limit. Plan
  # capacity is shared with the operator's own sessions, so a run that retries
  # straight into a limit takes capacity from the person it is working for.
  rateLimitBackoff: 1h
  # How many times a run may push a fix at failing CI before giving up and
  # taking the blocked label.
  fixAttempts: 3

slots:
  # Inclusive range. SlotBooks reserves lane 0 for the main stack, so the pool
  # starts at 1.
  min: 1
  max: 6
  # Run with CLAUDEFLOW_SLOT set. Exit 0 means the slot is busy. An error also
  # counts as busy: allocating a slot whose state is unknown risks two
  # environments on one port block, which is worse than waiting one tick.
  busyCheck: test -S .lanes/$CLAUDEFLOW_SLOT/overmind.sock

hooks:
  install: pnpm install --frozen-lockfile
  envUp: ./scripts/lane.sh up $CLAUDEFLOW_SLOT
  envDown: ./scripts/lane.sh down $CLAUDEFLOW_SLOT
  verify: pnpm verify:ci

agent:
  command: claude
  model: opus
  # extraArgs: []
  # skillsDir: ./my-skills   # overrides the skills shipped in the binary
  autoUpdate: true

paths:
  # The checkout runs are created from, and the one fast-forwarded after a
  # merge. Relative paths resolve against the config file's directory.
  root: /home/sj/github/common-pattern/slotbooks
  # Defaults to <root>/.claudeflow
  # state: /var/lib/claudeflow
  # Defaults to <state>/worktrees
  # worktrees: /var/lib/claudeflow/worktrees
```

### The hook contract

Every hook runs through `sh -c` with these variables in its environment:

| Variable | Value |
| --- | --- |
| `CLAUDEFLOW_SLOT` | the allocated slot integer |
| `CLAUDEFLOW_WORKTREE` | absolute path to the run's worktree |
| `CLAUDEFLOW_BRANCH` | the branch the run is working on |
| `CLAUDEFLOW_ISSUE` | the issue number, or the pull request number for a review run |

`busyCheck` receives `CLAUDEFLOW_SLOT` only — it is asked about a slot before
any run owns it.

Two requirements on `envDown`:

- **It must be safe to run twice.** claudeflow calls it on the normal path and
  again when reaping a run whose process died, and it cannot always tell which
  happened.
- **It must not destroy shared resources.** Slot environments are usually
  global to the machine, so a teardown that prunes "everything unused" will
  take out another slot's database or another checkout's containers. Scope the
  teardown to `$CLAUDEFLOW_SLOT`.

If `envUp` is set, `envDown` is required. Configuration without it is rejected
at load, because it would leak one environment per run.

### What a slot is

A slot is an integer. What it means is the project's business: a port block and
a database, a container set, a cloud namespace, a set of Kubernetes resources.
claudeflow picks a number nothing else is using and passes it to the hooks; it
has no idea what the hooks do with it.

`busyCheck` exists because slot resources are usually global to the machine
rather than to one checkout. Another checkout, or a developer working by hand,
can be holding a slot that claudeflow's own records know nothing about. Asking
the machine is the only way to see that.

## Commands

**Early — this interface may change.**

| Command | What it does |
| --- | --- |
| `claudeflow serve` | the supervisor: ticks on a loop, dispatches runs, reaps finished ones |
| `claudeflow once` | run a single tick and exit. Use this to try a configuration before running `serve` |
| `claudeflow status` | what is live, which slots are held, whether dispatch is paused |
| `claudeflow pause` | stop dispatching new runs. Live runs continue |
| `claudeflow resume` | undo `pause` |
| `claudeflow stop` | stop live runs |
| `claudeflow labels` | create or update the six labels in the repository. Run once per repo |
| `claudeflow version` | version, commit and build date |

A first run:

```sh
claudeflow labels          # create the label set in the repo
gh issue edit 412 --add-label claude
claudeflow once            # one tick, in the foreground
claudeflow status
```

## Why it merges rather than stopping at a pull request

Stopping at a green pull request looks safer and is not. The operator tests the
integration branch — that is the branch their environment tracks, the one they
open in a browser, the one they run against. Work parked on a green PR branch
is invisible there. It accumulates, the branches drift from each other and from
the integration branch, and the person who was supposed to review it never sees
it in the only place they look.

So claudeflow merges into the integration branch and leaves the
integration-to-base merge to a human. The operator tests one branch, and
anything that reached it is something they can see. `branches.base` is never
written to by the agent.

## Licence

MIT. See [LICENSE](LICENSE).
