---
name: claudeflow-resolve
description: The pipeline for an unattended run sent at a green pull request that conflicts with the integration branch — merge the integration branch in, resolve the conflicts, verify, push, and stop. Invoked by claudeflow; not for interactive use.
---

# Unattended conflict resolution run

A pull request you opened passed its checks, but when claudeflow went to land
it, the integration branch had moved on and no longer merges cleanly with it.
Merge the integration branch in, resolve the conflicts, and stop.

| variable | meaning |
|---|---|
| `CLAUDEFLOW_PR` | the pull request that conflicts |
| `CLAUDEFLOW_ISSUE` | the issue it implements |
| `CLAUDEFLOW_ATTEMPT` | which attempt this is |
| `CLAUDEFLOW_REPO` | `owner/name` |
| `CLAUDEFLOW_BRANCH` | your branch, already checked out |
| `CLAUDEFLOW_INTEGRATION` | the branch the pull request merges into |
| `CLAUDEFLOW_WORKTREE` | your worktree, already your working directory |
| `CLAUDEFLOW_SLOT` | your environment slot, already brought up |
| `CLAUDEFLOW_VERIFY` | the project's full check command |

The project's own agent instructions are loaded and govern anything specific to
the project.

## This run has no second turn

You are a single non-interactive invocation. When you stop producing output the
run is over: nothing resumes you, and every process in your group is killed
with you.

**Do not wait for checks after pushing.** That is claudeflow's job. Push the
merge and stop. When the checks go green it lands the pull request; if they go
red you or another run will be started at the failure.

**Nothing you leave on this machine outlives you either.** Your checkout may be
thrown away the moment you stop, and the next run starts from the branch as it
was pushed. But this branch already has an open pull request, and every push to
it runs the checks again. So push a finished change, never work in progress. If
you stop without one, put what you found on the issue, in enough detail that the
next run does not have to find it again.

## Sign every comment you post

End every comment you write with this line, exactly:

```
<!-- claudeflow:agent -->
```

You post through the operator's own credentials, so without it the system reads
your own words as a fresh instruction from them.

## 1. Merge the integration branch in

```
git fetch origin
git merge "origin/$CLAUDEFLOW_INTEGRATION"
```

Merge, do not rebase. The branch is already pushed and the pull request points
at it; rewriting it would throw away the history the checks ran against and
needs a force push, which you must not do.

If the merge completes without conflicts, someone already resolved it or the
integration branch moved again. Go straight to step 3.

## 2. Resolve each conflict on its merits

Read what both sides were for before choosing between them. The integration
branch's side is work that has already landed; your side is the work this pull
request exists for. The usual answer keeps both: the landed change, with this
pull request's change re-applied on top of it.

- Look at the commits that touched each conflicted file on the integration
  branch (`git log -p "HEAD..origin/$CLAUDEFLOW_INTEGRATION" -- <file>`) so you
  know what they changed and why.
- Never resolve by taking one side wholesale to make the markers go away. That
  silently reverts landed work, or silently drops this pull request's.
- Generated files and lockfiles: regenerate them with the project's own tool
  rather than editing the merge result by hand.
- A migration or schema change on both sides needs its ordering checked, not
  just its text merged.

If a conflict needs a decision only the operator can make, because the two
sides want incompatible behaviour, abort the merge (`git merge --abort`),
explain the conflict on issue `$CLAUDEFLOW_ISSUE`, and stop.

## 3. Verify

```
$CLAUDEFLOW_VERIFY
```

The merge produced a tree nobody has tested. It is not done until the full
check passes here. Fix whatever the combination broke, under the project's own
rules.

## 4. Push and stop

Commit the merge with its default message (`git commit --no-edit`), plus any
follow-up fixes as their own commits. Commit only the files you touched. Never
`git add -A`, never `git stash`.

```
git push origin "$CLAUDEFLOW_BRANCH"
```

Then comment on issue `$CLAUDEFLOW_ISSUE` saying which files conflicted and how
you resolved each, and stop. Do not watch the checks, do not merge, do not
touch labels: claudeflow is watching.
