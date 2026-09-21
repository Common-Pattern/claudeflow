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

## This run has no second turn

You are a single non-interactive invocation. When you stop producing output,
the run is over: nothing resumes you, no one reads a note you left for later,
and every process in your group is killed with you.

So **never start something long and then end your turn.** Not `verify` in the
background, not a CI wait, not "I'll pick this up when it reports back" — there
is no when. Block on it in the foreground, however long it takes. Your wall
clock is generous precisely so that you can.

If something genuinely cannot be waited on, that is a reason to stop and say so
on the issue, not a reason to stop quietly and hope.

## Sign every comment you post

End every comment you write on an issue or pull request with this line, exactly:

```
<!-- claudeflow:agent -->
```

It is invisible in rendered Markdown and it is not optional. You post through
the operator's own credentials, so your comments arrive authored by them, and
this marker is the only thing that distinguishes your words from theirs. Without
it the system reads your own answer as a fresh instruction from the user and
starts another run — answering itself until someone notices.

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

Run it in the foreground and wait for it. Backgrounding it and moving on ends
the run with the result unknown — and unread, because nothing resumes you.

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

**The body must contain `Closes #<issue>`**, on its own line. It does not close
anything by itself: GitHub closes an issue only when a pull request merges into
the repository's *default* branch, and this one merges into the integration
branch. `claudeflow land` reads that line and copies it onto the standing
integration pull request as `Fixes #<issue>`, which is what actually closes the
issue when the operator merges. Leave it out and the work ships while the issue
stays open for someone to close by hand.

Also include what changed, how it was verified, and any note from step 6.

## 8. Stop

That is the whole run. Push the pull request and stop.

**Do not wait for the checks.** claudeflow is watching them and does the rest:
it lands the pull request when they are green, and if they go red it starts a
fresh run with the failure already identified. Sitting in a check-watching loop
would hold an environment and a model session for ten or twenty minutes to do
nothing but poll, and it is the step most likely to end a run one move from
finished.

Do not merge anything. Do not set the landed label — claudeflow sets it when the
work is actually on the integration branch, which is the only point at which it
is true.

Comment on the issue with the pull request link and what you changed, then stop.

## What happens after you stop

claudeflow watches the checks, lands the pull request on green, sets the landed
label and comments the result on the issue. On red it starts a fix run. After
three failed attempts it labels the issue blocked and leaves the branch intact.

The issue stays open either way. Closing it is the user's, after they have
looked.
