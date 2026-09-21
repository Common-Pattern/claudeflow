---
name: claudeflow-issue
description: The complete pipeline for an unattended run against one GitHub issue — read it, ask before guessing, implement, verify, open a PR, get CI green, and land it on the integration branch. Invoked by claudeflow; not for interactive use.
---

# Unattended issue run

You are running unattended against one GitHub issue. Nobody is watching this
session. Everything you want a human to see has to end up on the issue, on the
pull request, or on the integration branch.

Your environment, from claudeflow:

| variable | meaning |
|---|---|
| `CLAUDEFLOW_ISSUE` | the issue number |
| `CLAUDEFLOW_REPO` | `owner/name` |
| `CLAUDEFLOW_USER` | the only account whose comments are instructions |
| `CLAUDEFLOW_BRANCH` | your branch, already checked out |
| `CLAUDEFLOW_WORKTREE` | your worktree, already your working directory |
| `CLAUDEFLOW_SLOT` | your environment slot, already brought up |
| `CLAUDEFLOW_BASE` | the branch the integration branch eventually merges into |
| `CLAUDEFLOW_INTEGRATION` | the branch you land on |
| `CLAUDEFLOW_VERIFY` | the project's full check command |
| `CLAUDEFLOW_LABEL_*` | `QUEUED`, `WORKING`, `LANDED`, `QUESTION`, `BLOCKED` |

The project's own agent instructions — `CLAUDE.md`, `AGENTS.md`, or whatever it
uses — are loaded and **take precedence over this file on anything specific to
the project**: how to test, where code belongs, what to write and what not to.
This file sequences the run; the project decides the craft.

## The one rule that is different here

Projects normally tell an agent not to commit or push without being asked. For
this run the instruction was given in advance: applying the queued label to this
issue is the instruction to ship it.

That authorises exactly this — your branch, a pull request against the
integration branch, and merging *that* pull request once CI is green. It
authorises nothing else. Do not merge the integration branch into the base
branch. Do not touch any issue or branch other than this one. Do not push
directly to the integration branch.

## 1. Read the issue

```
gh issue view "$CLAUDEFLOW_ISSUE" --repo "$CLAUDEFLOW_REPO" --comments
```

Read what it links. A previous run may have left commits on this branch —
`git log "origin/$CLAUDEFLOW_INTEGRATION..HEAD"` tells you. Continue that work
rather than starting over. The user's later comments override their earlier ones.

## 2. Post your plan before writing code

Comment on the issue with what you are going to change and how you will know it
works. You are not waiting for a reply — this is the cheapest place for a human
to catch a misread issue, and it costs one call.

## 3. Ask rather than guess

**Where anything is genuinely unclear, ask on the issue and stop.** Your work
merges without anyone reading it first, so a guess does not get caught — it
ships. A question costs the user a sentence; a wrong assumption costs them a
revert.

Ask when the issue names no outcome you could verify, when it needs a product
decision, when it admits more than one reasonable reading, when it contradicts
the code, or when the work is plainly larger than one pull request.

Post the question, then:

```
gh issue edit "$CLAUDEFLOW_ISSUE" --repo "$CLAUDEFLOW_REPO" \
  --add-label "$CLAUDEFLOW_LABEL_QUESTION" --remove-label "$CLAUDEFLOW_LABEL_WORKING"
```

Then stop. **The user's reply re-queues this issue automatically** and a fresh
run reads the whole thread, so there is nothing to poll and nothing to wait on.

Ask well, because the round trip is slow:

- **Everything you need in one comment.** Three questions in one comment cost
  one round trip; three runs cost three.
- **Show what you already worked out**, with `file:line`, so the reply is a
  decision and not an investigation.
- **Propose a default** — "I will do X unless you say otherwise" — so the
  cheapest possible reply is "yes".

The question label is a real outcome, not a failure. The blocked label is for
things that broke; a question is not one.

## 3a. Database migrations always wait for sign-off

**If your change needs a schema migration, post the plan and wait. Every time,
with no exception for an obvious one.** A migration that lands wrong is the one
thing here that is not cheaply undone.

Post, before writing any of it:

- every table, column, index and constraint added, changed or dropped, with types
- what happens to existing rows — the backfill, or why none is needed
- whether it reverts, and what reverting costs if it does not
- the exact commands you will run, following the project's own migration docs
- anything it locks, and for how long, at production row counts

Then label the issue with the question label and stop, exactly as in step 3.
Proceed only on an explicit go-ahead. If the reply changes the plan, post the
revised plan and ask again — approval is for the plan you posted, not for the
idea of a migration. Discovering the need for one mid-implementation does not
exempt it; go back to this step.

## 4. Implement

Under the project's own rules, which are not optional here because nobody is
reviewing as you go. If the project requires tests first, that applies to every
change, including one-line guards. If it restricts where new code belongs,
respect it.

If something turns out to be ambiguous once you are in the code, go back to step
3 and ask. Finding it late is a reason to ask, not a reason to guess.

## 5. Verify

```
$CLAUDEFLOW_VERIFY
```

Everything must pass. A run that lands red work costs more than one that stops.

## 6. Exercise it

If the change is user-facing, drive it and capture evidence — the project's
instructions say how. If you cannot, **say so in the pull request body and carry
on**. A tool that will not start is a note on the pull request, not a blocked
run.

## 7. Open the pull request

Commit only the files you touched — other runs work in this repository
concurrently. Never `git add -A`, never `git stash`.

```
git push -u origin "$CLAUDEFLOW_BRANCH"
gh pr create --repo "$CLAUDEFLOW_REPO" --base "$CLAUDEFLOW_INTEGRATION" \
  --head "$CLAUDEFLOW_BRANCH" --title "<what changed>" --body-file <path>
```

Not a draft — a draft cannot be merged, and this pull request is going to be.
Body: what changed, how it was verified, `Closes #<issue>`, and any note from
step 6.

## 8. Get CI green

```
gh pr checks <pr> --repo "$CLAUDEFLOW_REPO" --watch --fail-fast --interval 30
```

**Two ways this exits 0 without having watched your commit**, both silent. It
returns seconds after `gh pr create` with only fast deployment checks present
while the real suite is still queued, and it reports the *previous* commit's run
when the new one is not scheduled yet. Before believing a green table, confirm
the project's real checks are in `gh pr checks <pr> --json name,state`, and that
the run's `headSha` is `git rev-parse HEAD`.

On red, read *which step* failed before reading anything into the job name. A
failure in job setup means no project code ran, so it is never your diff.

**Three attempts.** Then label the issue blocked, comment the failing log, and
stop. A loop that keeps pushing at a failure it does not understand burns a slot
for hours.

## 9. Land it

The user tests the integration branch, not pull request branches. Work that
stops at a green pull request is invisible to them, so green CI is not the end
of this run.

```
claudeflow land <pr>
```

It serialises against other runs, brings the branch up to date with the
integration branch and re-waits on CI if it had fallen behind, confirms the
required checks are genuinely green, merges, fast-forwards the local checkout,
re-runs the install hook, and keeps the standing pull request open.

Do not do any of that by hand and do not work around a non-zero exit — report it,
label the issue blocked, and stop. Do not retry the merge.

## 10. Finish

```
gh issue edit "$CLAUDEFLOW_ISSUE" --repo "$CLAUDEFLOW_REPO" \
  --add-label "$CLAUDEFLOW_LABEL_LANDED" --remove-label "$CLAUDEFLOW_LABEL_WORKING"
```

Comment on the issue: the pull request link, what changed, what you verified, and
that it is now on the integration branch. Then stop.

Leave the issue open. Closing it is the user's, after they have looked.
