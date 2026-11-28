# claudeflow

claudeflow drives a coding agent from GitHub issue labels. Label an issue, and
it claims the issue, creates a git worktree, brings up an isolated environment
the project defines, and runs the agent CLI against the issue. The agent opens a
pull request and stops there; claudeflow watches the checks, merges into the
integration branch when they are green (or, with `merge: human`, leaves it for
you to review and revises it on your comments), and sends a fresh agent at the
failure when they are not.

A green pull request that has fallen behind the integration branch is brought
up to date before it merges. When the integration branch merges in cleanly,
claudeflow pushes the merge and waits for the new head's checks. When it
conflicts, claudeflow sends an agent to merge it in, resolve the conflicts and
push, and then waits for the checks again. Conflicts and red checks share the
`fixAttempts` budget.

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

## Contributing

The checks CI runs are gofmt, `go vet`, `golangci-lint`, `go test -race` and
`go build`. To run the same ones before each commit:

```sh
git config core.hooksPath .githooks   # once per clone
```

git deliberately will not install a hook for you — a repository that ran code
on checkout would be a way to attack anyone who cloned it — so the hook is
opt-in, and CI is what actually enforces the result. The hook skips
`golangci-lint` with a note if it is not installed, and `--no-verify` overrides
it.

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

## Creating the GitHub App

> **Not used yet.** claudeflow still acts through `gh` and your own account
> (see Prerequisites). Moving it onto a GitHub App is tracked in
> [#10](https://github.com/Common-Pattern/claudeflow/issues/10). This section is
> how to prepare the app, so the switch is a config change once it lands.

With an app, claudeflow comments, opens pull requests and merges as
`<app-name>[bot]` instead of as you. Your account then says only what you
wrote, and you can approve claudeflow's pull requests, which GitHub does not let
you do for pull requests opened in your own name. The app holds no long-lived
token. claudeflow mints an hour-long installation token from the app's private
key when it needs one, narrowed to the repository and the permissions that each
run needs.

**Make the app private, and make one per owner.** A private app can only be
installed on the account that owns it. Repositories under an organization and
repositories under your personal account therefore need an app each. That also
keeps the two apart: no token from one app can reach the other owner's
repositories.

1. **Open the form.** For an organization, go to
   `https://github.com/organizations/<org>/settings/apps/new`. For your personal
   account, go to `https://github.com/settings/apps/new`. Both are under
   Settings → Developer settings → GitHub Apps → New GitHub App.
2. **Name it** something like `claudeflow-<owner>`. App names are unique across
   GitHub, and the bot posts as `<name>[bot]`. The homepage URL is required but
   unused, so the repository's URL will do.
3. **Leave user authorization off.** Leave the callback URL empty, and do not
   tick *Request user authorization (OAuth) during installation*. claudeflow
   acts as the app, never as a user signing in through it.
4. **Turn the webhook off.** Untick *Webhook → Active*, because claudeflow polls.
5. **Set the repository permissions** below. Leave everything else at *No
   access*.
6. Under **Where can this GitHub App be installed?**, choose **Only on this
   account**.
7. **Create it.** Note the **App ID** and the **Client ID** on the page that
   follows. Runner management uses the Client ID.
8. **Generate a private key**, under *Private keys*. Store it where only you can
   read it:

   ```sh
   install -m 0600 ~/Downloads/<app-name>.*.private-key.pem ~/.config/claudeflow/keys/<owner>.pem
   ```

   The key is the app's only credential, so keep it out of every repository.
   GitHub keeps no copy, so if you lose it, generate another and delete the
   old one.
9. **Install the app.** Go to *Install App*, pick the owner, and choose *Only
   select repositories*. Select the projects claudeflow drives. Adding a project
   later means adding it to this installation too.

### Repository permissions

| Permission | Access | What claudeflow does with it |
| --- | --- | --- |
| Contents | Read and write | clones, pushes run branches, compares, merges, deletes merged branches |
| Pull requests | Read and write | opens, edits and merges pull requests; comments and replies to review comments |
| Issues | Read and write | reads issues, sets and creates labels, comments, reads issue events |
| Workflows | Read and write | lets a run push changes under `.github/workflows/`; GitHub rejects those pushes without it |
| Actions | Read | lets the fix skill read failed CI logs |
| Checks | Read | reads a pull request's checks |
| Commit statuses | Read | reads a pull request's checks |
| Metadata | Read | mandatory; GitHub selects it for you |

Workflows is the only one you might leave out. Without it a run cannot change
CI, and the run fails at the push, not quietly.

To have claudeflow start self-hosted Actions runners on demand as well
([#11](https://github.com/Common-Pattern/claudeflow/issues/11)), also grant
the following. Each is optional and does nothing until that feature exists.

| Permission | Access | When |
| --- | --- | --- |
| Self-hosted runners (organization) | Read and write | runners registered to an organization |
| Administration (repository) | Read and write | runners registered to a single repository, which is the only kind a personal account has |

Administration covers far more than runners, such as repository settings and
collaborators. Grant it only on an app installed on repositories you want
claudeflow to host runners for.

Changing an app's permissions later does not take effect straight away. Each
installation has to accept the change: GitHub asks the owner under the
installation's settings.

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
| `claude:review` | the pull request is green and waiting for you (`merge: human` only) | claudeflow |
| `claude:planning` | alongside `claude`, inverts the run into conversation only | a human |

`claude:planning` produces no branch, no commits and no files. The agent reads
the repository and thinks in the issue: its comments are the only artefact of
the run. Planning runs hold no environment slot and are budgeted separately
from builds, so a conversation does not queue behind a two-hour build.

### A blocked-by link holds the issue

claudeflow will not start a build on a queued issue that GitHub records as
*blocked by* another issue whose work is unfinished. A blocker counts as
finished once it is closed, or once it carries `claude:landed`. Its work is then
on the integration branch, and that is the branch the next run starts from. A
blocker in another repository counts as finished only once it is closed. The
issue keeps `claude` and starts on the first tick after its last blocker is
done; `claudeflow status` shows what each queued issue is waiting on.
Planning runs are not held.

Dependencies are yours to record. An agent that finds an unrecorded
prerequisite asks about it on the issue rather than adding one.

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

### A comment during a run reaches the run

A run reads its thread when it begins, but you can keep talking. A comment on
the issue while a run is working — or on its pull request, when you are the
one who merges — is handed to the agent within a tick (seconds, with
`serve -watch`), at its next tool call. If it lands after the agent's last
tool call, the agent is turned back from finishing until it has read it. The
agent is told such a comment outranks its plan.

This goes through the agent CLI's hooks, passed with `--settings` as each run
starts; nothing is written into the checkout. Each tick leaves new comments in
`<state>/inbox/<run>/`, and the hooks run `claudeflow inbox`, which only reads
files. A comment the agent never saw — the run had already ended, or was
waiting on checks with no agent alive — is still acted on afterwards, as if
the run had not been there: the issue is re-queued, or the pull request
revised.

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
  review: claude:review
  planning: claude:planning

# Who merges a green pull request. "auto" (the default): claudeflow lands it on
# the integration branch. "human": it waits for you, and your comments and
# reviews on it start a revision on the same branch. See "Leaving the merge to
# a human". Either way, once it has merged, the run's branch (under
# branches.prefix) is deleted from GitHub.
merge: auto

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
  # How many times an agent is sent at failing checks, or at a conflict with
  # the integration branch, before the issue is blocked and the branch left
  # for a human.
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
  # Appended to every run's instructions, of every kind, under "About this
  # project". For what only holds under claudeflow — the environment a run is
  # given, and which interactive tooling to leave alone. Conventions that hold
  # for anyone working in the repository belong in its CLAUDE.md instead.
  # promptNote: |
  #   Your environment is the Compose stack claudeflow brought up for you.
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

# Where runs happen. "local" (the default) runs every agent on this host with
# your own access. "container" gives every run a fresh container: its own clone,
# its own stack on a podman nested inside it, and the agent. See "Running every
# run in a clean sandbox".
sandbox:
  kind: local
  # image: localhost/claudeflow-sandbox
  # env: [CLAUDE_CODE_OAUTH_TOKEN]   # optional: runs share this host's Claude login without it

# Optional: a directory of data a project script prepares on the host, given
# to every run read-only as CLAUDEFLOW_DATA. See "Giving every run prepared data".
# data:
#   dir: ~/.local/share/claudeflow/myproject
#   script: scripts/prepare-run-data.sh
#   maxAge: 24h    # the oldest data a run may be given; the default
```

### Serving transcripts

Every run comments on its issue as it starts, naming its transcript, and the
comment that settles it names the transcript again. Without this each names a
path — `/home/sj/.../.claudeflow/logs/build-252-20260922T165737.log` — which is
only actionable to someone already logged in to the host that produced it.

Setting a port makes `serve` publish the log directory read-only over HTTP, and
the comments carry a link instead. Runs write the agent's event stream to the
log as they work, so the link opens a page that follows the run live: the
prompt and system prompt it was given (collapsed, at the top), its messages,
each tool call and its output, and how it finished. `?raw` is the
file itself. Nothing is copied anywhere: the file is served where housekeeping already manages it, so
the retention window is the one you already set. The alternatives all involve copying — a gist is unlisted
rather than private, and a branch puts run output into the project's history.

```yaml
transcripts:
  host: bigone.your-tailnet.ts.net
  port: 8787
  allow: [100.64.0.0/10, 'fd7a:115c:a1e0::/48']
```

- **`allow` is required.** Transcripts hold whatever the run printed: the
  project's code, its test output, sometimes production-derived data. There is
  no default, and deliberately no permissive one — serving every run's output to
  whoever asked is not something to arrive at by leaving a field out.
- **`host` is both the bind address and the hostname links are built from.**
  One field, because two drift, and the failure then looks like the server being
  down. It is required: an empty host binds every interface, and the difference
  between "reachable on one private network" and "reachable on every network this
  machine is attached to" is too large to be the consequence of omitting a line.
  Bind the interface you mean — a host that should only answer over a tailnet
  binds its tailnet address, and `allow` is then the second line of defence
  rather than the only one.
- **Use a fully qualified name.** On a Tailscale host the short hostname is
  usually in `/etc/hosts` as `127.0.1.1`, so the server would bind loopback while
  the links it hands out resolve, on another device, to the tailnet address.
  Nothing errors — the server is up, the links are well-formed, and every one of
  them times out. `claudeflow doctor` resolves the name and compares it to
  `allow` precisely to catch this.

The server has two routes, a listing and a file. It refuses anything outside
`allow` by the connection's own peer address; `X-Forwarded-For` is a claim by
the caller and is not trusted, because nothing here sits behind a proxy that
could make it a fact. It serves only `*.log` basenames from the log directory,
as `text/plain` with `nosniff`.

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

Every hook, the agent and the Compose file also get `CLAUDEFLOW_DATA` when
`data` is set: see the next section.

### Giving every run prepared data

Some things are too big or too slow to make inside every run. claudeflow can
keep a directory of them fresh and hand it to every run:

```yaml
data:
  dir: ~/.local/share/claudeflow/myproject
  script: scripts/prepare-run-data.sh
  maxAge: 24h
```

The split is deliberate. The project owns the script, what it puts in the
directory, and what a run should do with it, which it tells the agent in
`agent.promptNote`. claudeflow owns only keeping the directory younger than
`maxAge` and handing it over; it knows nothing about what is inside.

- **Preparing.** `claudeflow serve` runs the script on the host, in
  `paths.root`, so that the data a run is given is never older than `maxAge`.
  It starts the script ahead of that deadline by twice as long as its last run
  took, or by a quarter of `maxAge` before there is a last run.
  `claudeflow data prepare` runs it now. The script is given an empty
  directory in `CLAUDEFLOW_DATA_OUT` to fill, is stopped once it has run for
  `maxAge`, and leaves its output in `<dir>/prepare.log`.
- **Publishing.** Only a run of the script that exits zero is published: its
  directory becomes a new generation under `<dir>/generations/`, and
  `<dir>/current` is swung to it atomically. A failure leaves the previous
  generation in place and is retried after a quarter of `maxAge` or half an
  hour, whichever is sooner. `status` and `doctor` say when the data is past
  `maxAge`. Runs never see a half-written directory.
- **Using it.** A run gets the generation that was current when it started, as
  `CLAUDEFLOW_DATA` — in the agent's environment, the hooks', and the Compose
  file's interpolation — and keeps it for its whole life. A container sandbox
  mounts it read-only at the same path. Before anything has been prepared,
  `CLAUDEFLOW_DATA` is an empty directory.
- **Cleaning up.** A generation is removed once it is neither current, the one
  before it, nor recorded against a run.
- **Containment.** `dir` is made `0700`, and what is inside a generation
  readable by anyone, so that a container's own users can read it. Anything
  that must not reach a run belongs in the script's own temporary files, not
  in `CLAUDEFLOW_DATA_OUT`.

### What a slot is

A slot is an integer. What it means is the project's business: a port block and
a database, a container set, a cloud namespace. claudeflow picks a number
nothing else is using and hands it to the Compose file.

Whether a slot is free is answered by asking Compose whether that project has
containers, so there is nothing to configure — and it sees a stack another
checkout left running, which claudeflow's own records cannot.

### Running every run in a clean sandbox

With `sandbox.kind: container`, a run happens in a new container instead of a
worktree on this host. The container clones the run's branch from GitHub, runs
the install hook, brings the Compose file's stack up on a rootless podman
*inside* the container, and starts the agent there. When the agent exits the
container is deleted, along with the checkout, the stack and anything the agent
did to either. The next run on that slot starts from nothing but what was
pushed.

What crosses back to the host is the run's transcript, its comment inbox, and
the branch it pushes. That is also the whole of what the agent can reach on
this machine: it does not see your home directory, your other checkouts, or
your credentials beyond the ones handed in and the `~/.claude` directory it
shares with this host.

**Requirements.** Rootless podman, and an image carrying git, podman with a
compose provider, and the agent CLI. `sandbox/Containerfile` is the reference
image; build it, then add your project's toolchain in an image of your own
`FROM` it:

    podman build -t localhost/claudeflow-sandbox sandbox/

**Credentials.** GitHub's token goes in automatically: `GH_TOKEN`, else
`GITHUB_TOKEN`, else whatever `gh auth token` prints.

Claude Code needs nothing either, as long as this host is logged in to Claude.
Every run mounts this host's `~/.claude` at the same path and runs with
`CLAUDE_CONFIG_DIR` pointing at it, so a sandboxed agent uses the login exactly
as another `claude` process on this host would. That directory also holds the
locks the CLI takes to refresh the login, so whichever process refreshes it,
host or sandbox, does so under the same lock, and every other process sees the
new login on its next request. Session transcripts land in `~/.claude/projects`
as the run writes them, so local history and `/stats` include sandboxed runs.

The CLI's global config is the one thing kept apart. The host keeps it in
`~/.claude.json`, next to a lock in your home directory that a container cannot
share. With `CLAUDE_CONFIG_DIR` set, the CLI uses `~/.claude/.claude.json`
instead, so sandboxed runs share that one between them. Before each run
claudeflow copies the host's account and identifiers into it (`oauthAccount`,
`userID`, `machineID`), so runs report to Anthropic as the host does. Your MCP
servers, per-project state and caches in `~/.claude.json` stay the host's.

A run also uses the host's own `claude` binary, mounted read-only, when that is
a native install, so its version is the host's. `claude auth status` inside a
run reads as it does on the host.

The sandbox isolates workloads; it is not a security boundary. A run can read
and change everything in `~/.claude`, as a local run can. Runs lose the login
when the host does: `claudeflow doctor` warns a day before the login itself has
to be renewed in a browser.

To give runs a credential of their own instead, name it in `sandbox.env`. That
can be a `CLAUDE_CODE_OAUTH_TOKEN` from `claude setup-token`, which lasts a year,
or an `ANTHROPIC_API_KEY`. It is passed by name, so no value appears in a
command line or a file, and the CLI uses it in preference to the shared login.

**What changes for a run.**

- The nested podman is given what systemd gives the host's: an API socket,
  with `DOCKER_HOST` pointing at it, and a runner for healthchecks. So the
  stack, and anything the agent brings up itself, can use Docker Compose
  (`podman compose` hands off to it when the image carries it, as the reference
  image does), and `condition: service_healthy` and claudeflow's own readiness
  wait see healthchecks pass. A run has no process limit beyond the operator's.
- Use the upstream Node binary in a project image rather than Fedora's
  `nodejs` packages: some native builds segfault on Fedora's.
- The image's podman must be 6.2.1 or later (or 5.8.11 on the 5.x line). Earlier
  6.x fails every exec into a container Docker Compose created, healthchecks
  included, because the rootless session's `oom_score_adj` of 200 cannot be
  lowered to the 0 Compose asks for. The reference image makes sure of it.
- A `sandbox.mounts` source that does not exist yet is created as a directory,
  as Docker does. That is what lets a run share a tool's lock directory in
  `/run/user/<uid>`, which every boot empties. For pnpm, mount the host's store
  at the same path, and its lock directories from `/run/user/<uid>` at
  `/tmp/pnpm-store-operation-locks-<uid>` and `/tmp/pnpm-dir-locks-<uid>`, so the
  host's pnpm and a run's wait for each other.
- Each slot keeps its nested image storage in a volume, so a stack's images
  are pulled once per slot rather than once per run. Containers, volumes and
  networks in it are removed before every run.
- `compose.env` is not passed into the sandbox: it exists to point Compose at
  the host's engine, and there the engine is the nested one. Service addresses
  in `CLAUDEFLOW_URL_<SERVICE>` are on `localhost`, inside the sandbox.
- A run that stops without pushing loses what it did. The skills tell the agent
  to push before any stop on a branch with no pull request yet, and to write
  findings on the issue instead where a push would re-run checks.
- Landing works from a checkout of its own under `paths.worktrees`, reset to
  the pushed branch each time; no agent runs in it.

## Commands

**Early — this interface may change.**

| Command | What it does |
| --- | --- |
| `claudeflow init` | write a starter `claudeflow.yaml` here, filling in what the checkout and `gh` already know. Refuses to overwrite one |
| `claudeflow serve` | the supervisor: ticks on a loop, dispatches runs, reaps finished ones. Ticks every `-interval` (2m), and sooner when a cheap check every `-watch` (15s) sees an issue opened, labelled or commented on |
| `claudeflow once` | run a single tick and exit. Use this to try a configuration before running `serve` |
| `claudeflow status` | what is live, which slots are held, whether dispatch is paused |
| `claudeflow pause` | stop dispatching new runs. Live runs continue |
| `claudeflow resume` | undo `pause` |
| `claudeflow stop` | stop live runs |
| `claudeflow doctor` | check dependencies, authentication, versions and configuration. Non-zero if a run would not get far |
| `claudeflow land <pr>` | merge a green pull request and sync the checkout. Normally done for you |
| `claudeflow housekeep` | reclaim worktrees whose branch is merged. `--dry-run` to look first |
| `claudeflow data [prepare]` | show the generation of prepared data runs are given, or prepare a new one now |
| `claudeflow labels` | create or update the seven labels in the repository. Run once per repo |
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

The argument changes when every pull request has its own preview environment:
then the pull request *is* somewhere the operator looks. That is what
`merge: human` is for.

## Leaving the merge to a human

With `merge: human`, each issue becomes its own pull request that you review
before anything merges. This suits a project where every pull request gets its
own preview environment, so the thing to test is the pull request itself rather
than an integration branch. It is usually paired with `integration` equal to
`base`.

1. The build run opens its pull request and stops, as in the default mode.
2. claudeflow watches the checks and sends fix runs at red ones, as before.
3. On green it does **not** merge. It labels the issue `claude:review` and
   comments that the pull request is ready, once per commit.
4. A comment or review from `user` on the pull request, including an inline
   review comment or a "request changes" review, or a comment on the issue,
   starts a **revision run**. It gets a slot and the run's existing worktree,
   reads every thread, answers questions, pushes changes to the same branch and
   stops. Back to step 2.
   While it waits, claudeflow compares the branch with the integration
   branch. When the integration branch has moved past it, it says so on the
   pull request, once per head commit: the branch needs updating before it
   merges, and a comment starts a revision that merges it in.
5. When you merge, claudeflow labels the issue `claude:landed` and fast-forwards
   the checkout. When you close the pull request unmerged, it labels the issue
   `claude:blocked`; a comment on the issue starts it again.

A pull request waiting for review holds no slot and does not count against
`maxBuilds`, so unreviewed work never stops new work from starting. A revision
does count: it is a build in every way that matters. A revision asked for while
every slot is busy is remembered and starts when one frees up.

## Licence

MIT. See [LICENSE](LICENSE).
