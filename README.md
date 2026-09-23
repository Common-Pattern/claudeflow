# claudeflow

claudeflow drives a coding agent from GitHub issue labels. Label an issue, and
it claims the issue, creates a git worktree, brings up an isolated environment
the project defines, and runs the agent CLI against the issue. The agent opens a
pull request and stops there; claudeflow watches the checks, merges into the
integration branch when they are green, and sends a fresh agent at the failure
when they are not.

That split is deliberate. Waiting on CI is ten or twenty minutes of an
environment and a model session spent polling, and it is the step an agent is
most likely to get wrong — ending its turn with the work finished and unmerged.

It is a generic extraction of a system that has been running against a real
product repository, not a demonstration. The orchestration — the state machine,
the locking, the slot allocation, the merge rules — is fixed. Everything that
varies between projects is a hook or a config key.

> **Early. The command-line interface and the configuration schema may change
> between versions.** Pin a version if you depend on either.

## Prerequisites

Check everything at once:

```sh
claudeflow doctor
```

It exits non-zero if anything would stop a run, so it works as a precondition
in a script. Run it without a configuration to ask only "can this host run
anything"; run it beside a `claudeflow.yaml` to check that too.

| Requirement | Why | How to check |
| --- | --- | --- |
| `git` on PATH | worktrees, branches, merges | `git --version` |
| **a git committer identity** | landing creates a merge commit | `git config user.email` |
| `gh` on PATH | every GitHub call goes through it | `gh --version` |
| **`gh` authenticated** | claudeflow holds no token of its own | `gh auth status` |
| read/write access to the repository | claiming, commenting, merging | `gh repo view <owner/name>` |
| an agent CLI (`claude` by default) | does the actual work | `claude --version` |
| a Compose implementation | brings each run's environment up | `docker compose version` |
| a Compose file in the project | the environment contract; required | `compose.file` in the config |
| a git checkout at `paths.root` | every run's worktree is cut from it | `git -C <root> rev-parse --git-dir` |

Three of these are worth calling out, because each fails late — during a run,
long after the cause, with nobody watching:

- **The git identity.** Without `user.name` and `user.email`, landing fails with
  `Committer identity unknown` *after* the agent has done all of the work. A
  fresh container or CI runner is the usual place this bites.
- **`gh` authentication.** claudeflow holds no GitHub token. It shells out to
  `gh` because `gh` already holds the operator's credentials, refreshes them,
  and resolves the host — so the same binary works against github.com and an
  enterprise host with no extra configuration. The consequence: whatever
  `gh auth status` reports is exactly what claudeflow can do, and
  re-authenticating `gh` is how a permissions failure gets fixed.
- **The Compose implementation.** Not every one is equal. Some accept being
  invoked and then hang on the first real command, which presents as a run that
  never starts rather than as an error. `doctor` asks yours for its version
  rather than only finding it on PATH, and `compose.bin` names one explicitly
  where the engine's default is not the one you want.

### Staying current

`doctor` also reports what is old. It compares claudeflow and `gh` against
their latest releases and warns when either is behind, and says what the
compose implementation's current release is. Being behind is never a failure —
an old tool works until it does not — but a supervisor running a build from
before a fix is the one fault nobody goes looking for, because it looks
healthy.

Nothing here updates anything. The agent CLI has its own updater, and a
supervisor that reinstalls a tool mid-flight is a worse idea than one that says
the updater is off — so `doctor` asks `claude doctor` whether auto-updates are
on and warns when they are not. An agent CLI that never moves pins the model
the `opus` alias resolves to, its skills, and its bug fixes to whatever was
installed once.

`doctor` reports on the configuration itself too: which file it read, that it
parsed, and that it validated. A scaffolded config that still says `OWNER/NAME`
is a warning rather than a pass, because it is well-formed and wrong.

### The configuration is parsed strictly

A key claudeflow does not read is an error, not a comment. `autoupdate` for
`autoUpdate`, a setting indented under the wrong block, one left behind by a
version that stopped reading it: each is an intention written down that the
tool silently does not hold, and lenient parsing means it is only ever
discovered from behaviour, during a run. `doctor` reports the parser's own
complaint, with the line number.

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

Labels carry the whole state of an issue. There is no lease file, no database,
and no coordination between processes beyond GitHub itself. A tick that crashes
loses nothing, because the next tick reads the same labels back.

`claude` marks membership and is **never removed**: the issue does not stop
being the agent's while a run is on it. The other labels carry what is
happening. An issue is ready to pick up when it has `claude`, nothing running,
and no outcome.

| Label | Meaning | Applied by |
| --- | --- | --- |
| `claude` | this issue is the agent's. Applied once, never removed | a human |
| `claude:working` | claimed and running. Adding this label **is** the lock | claudeflow |
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

claudeflow reads one YAML file. Every key not shown has a default.

```yaml
# The repository, in owner/name form. Everything runs against this one repo.
repo: acme/widgets

# The single account whose comments are instructions. Required, and there is no
# default: the agent posts through this same account, so without it claudeflow
# would read the agent's own comments as fresh instructions and answer itself.
user: alice

branches:
  # Never written to by the agent. The integration branch eventually merges
  # here, and a human does that merge.
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
  # minutes, so a reply should not queue behind two long builds.
  maxPlanning: 2
  buildTimeout: 150m
  planTimeout: 30m
  # How long dispatch pauses after the agent CLI reports a usage limit. Plan
  # capacity is shared with the operator's own sessions, so a run that retries
  # straight into a limit takes capacity from the person it is working for.
  rateLimitBackoff: 1h
  # How many times an agent is sent at failing checks before the issue is
  # blocked and the branch left for a human.
  fixAttempts: 3

slots:
  # Inclusive range. Reserve low numbers for anything else on the machine that
  # uses the same resources.
  min: 1
  max: 4

# Required. The Compose file is the environment contract: claudeflow brings the
# stack up before a run and takes it down after, under its own project name per
# slot, so a teardown can only reach what that project owns.
compose:
  file: claudeflow/compose.yaml
  # Per-slot project name: "<prefix>-<slot>". This is the namespacing.
  projectPrefix: cf
  # Bounds every compose command and kills the process group when it fires. A
  # stuck provider must fail one run, not block the supervisor.
  readyTimeout: 10m
  # Name the implementation explicitly where the engine's default provider is
  # not the one you want. Preferable to changing the engine's host-wide
  # configuration, which everything else on the machine reads too.
  # bin: [docker-compose]
  # env:
  #   DOCKER_HOST: unix:///run/user/1000/podman/podman.sock
  # profiles: [spa]

  # Services whose published port the run needs to reach. The Compose file can
  # publish ephemerally; claudeflow reads back what each landed on and passes
  # CLAUDEFLOW_PORT_<SERVICE>, CLAUDEFLOW_HOST_<SERVICE> and
  # CLAUDEFLOW_URL_<SERVICE>. Nothing then does port arithmetic.
  expose:
    web: 3000
    postgres: 5432
  # What the agent should dial. Defaults to localhost; set it where a browser
  # runs on another machine.
  # host: dev.example.net

hooks:
  # Prepares a fresh worktree. Runs before the stack comes up, because a
  # Compose file that bind-mounts the worktree needs its dependencies present.
  install: pnpm install --frozen-lockfile
  # The project's full check suite, named here so the skills can refer to it
  # without knowing the project.
  verify: pnpm verify:ci

agent:
  command: claude
  model: opus
  # extraArgs: []
  # skillsDir: ./my-skills   # overrides the skills shipped in the binary
  # Extra environment for the run, expanded against the addresses above. This
  # is how a project points its own tooling at the run's containers. A test
  # harness that refuses to guess its database — the correct behaviour — has
  # nothing to be told without it, and the run can verify nothing.
  env:
    DATABASE_URL: postgresql://app:app@localhost:${CLAUDEFLOW_PORT_POSTGRES}/app_test

# Checks that must be present and green before a merge. An absent check counts
# as not passed, which is what stops a merge landing before CI was scheduled.
# Empty means "every check the pull request reports, and at least one".
requiredChecks:
  - Unit Tests
  - Lint

paths:
  # The checkout runs are created from, and the one fast-forwarded after a
  # merge. Relative paths resolve against the config file's directory and are
  # made absolute — a relative path is meaningless to a container bind mount.
  root: .
  # Defaults to <root>/.claudeflow
  # state: /var/lib/claudeflow
  # Defaults to <state>/worktrees
  # worktrees: /var/lib/claudeflow/worktrees
```

### The Compose file

Every project ships one. claudeflow runs it per slot under `<projectPrefix>-<slot>`,
which is what makes isolation structural rather than conventional: `down -v`
can only reach containers, networks and volumes belonging to that project.

It is interpolated with `CLAUDEFLOW_SLOT`, `CLAUDEFLOW_WORKTREE`,
`CLAUDEFLOW_BRANCH` and `CLAUDEFLOW_PROJECT`, plus anything in `compose.env`.

Two things worth knowing:

- **Bind-mount the worktree at its own absolute path** if the containers are to
  use dependencies installed on the host. Package managers that build trees of
  symlinks resolve absolute links only when both sides agree on the path.
- **claudeflow polls readiness itself** rather than passing `up --wait`, because
  not every Compose implementation accepts that flag. A service with no
  healthcheck counts as ready once running; one with a healthcheck must report
  healthy.

### The hook contract

Both hooks run through `sh -c` in the worktree, with these in the environment:

| Variable | Value |
| --- | --- |
| `CLAUDEFLOW_SLOT` | the allocated slot integer |
| `CLAUDEFLOW_WORKTREE` | absolute path to the run's worktree |
| `CLAUDEFLOW_BRANCH` | the branch the run is working on |
| `CLAUDEFLOW_ISSUE` | the issue number, or the pull request number for a review run |

There is no environment hook. Bringing the stack up and down is claudeflow's
job through the Compose file — a shell hook could name any command, including
one that reaches another run's resources.

### What a slot is

A slot is an integer. What it means is the project's business: a port block and
a database, a container set, a cloud namespace. claudeflow picks a number
nothing else is using and hands it to the Compose file.

Whether a slot is free is answered by asking Compose whether that project has
containers, so there is nothing to configure — and it sees a stack another
checkout left running, which claudeflow's own records cannot.

## Commands

**Early — this interface may change.**

| Command | What it does |
| --- | --- |
| `claudeflow init` | write a starter `claudeflow.yaml` here, filling in what the checkout and `gh` already know. Refuses to overwrite one |
| `claudeflow serve` | the supervisor: ticks on a loop, dispatches runs, reaps finished ones |
| `claudeflow once` | run a single tick and exit. Use this to try a configuration before running `serve` |
| `claudeflow status` | what is live, which slots are held, whether dispatch is paused |
| `claudeflow pause` | stop dispatching new runs. Live runs continue |
| `claudeflow resume` | undo `pause` |
| `claudeflow stop` | stop live runs |
| `claudeflow doctor` | check dependencies, authentication, versions and configuration. Non-zero if a run would not get far |
| `claudeflow land <pr>` | merge a green pull request and sync the checkout. Normally done for you |
| `claudeflow housekeep` | reclaim worktrees whose branch is merged. `--dry-run` to look first |
| `claudeflow labels` | create or update the six labels in the repository. Run once per repo |
| `claudeflow version` | version, commit and build date |

A first run:

```sh
claudeflow init            # writes claudeflow.yaml; fill in what it could not detect
                           # then write the Compose file it names
claudeflow doctor          # dependencies, auth, versions, config
claudeflow labels          # create the label set in the repo
gh issue edit 412 --add-label claude
claudeflow once            # one tick, in the foreground
                           # run it again to advance a run waiting on checks
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
