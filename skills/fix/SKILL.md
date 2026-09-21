---
name: claudeflow-fix
description: The pipeline for an unattended run sent at a pull request whose checks went red — read the failure, fix it, push, and stop. Invoked by claudeflow; not for interactive use.
---

# Unattended fix run

A pull request you opened has failing checks. claudeflow watched them, saw the
failure, and started you with it already named. Fix it and stop.

| variable | meaning |
|---|---|
| `CLAUDEFLOW_PR` | the pull request with failing checks |
| `CLAUDEFLOW_ISSUE` | the issue it came from |
| `CLAUDEFLOW_FAILING_CHECKS` | which checks failed |
| `CLAUDEFLOW_ATTEMPT` | which attempt this is |
| `CLAUDEFLOW_REPO` | `owner/name` |
| `CLAUDEFLOW_BRANCH` | your branch, already checked out |
| `CLAUDEFLOW_WORKTREE` | your worktree, already your working directory |
| `CLAUDEFLOW_SLOT` | your environment slot, already brought up |
| `CLAUDEFLOW_VERIFY` | the project's full check command |

The project's own agent instructions are loaded and govern anything specific to
the project.

## This run has no second turn

You are a single non-interactive invocation. When you stop producing output the
run is over: nothing resumes you, and every process in your group is killed
with you.

**Do not wait for checks after pushing.** That is claudeflow's job — it started
you precisely because it was already watching. Push your fix and stop. If the
checks go red again you will be started again, with the new failure.

## Sign every comment you post

End every comment you write with this line, exactly:

```
<!-- claudeflow:agent -->
```

You post through the operator's own credentials, so without it the system reads
your own words as a fresh instruction from them.

## 1. Find out what actually failed

```
gh pr checks "$CLAUDEFLOW_PR" --repo "$CLAUDEFLOW_REPO" --json name,bucket,link
gh run view <run-id> --log-failed
```

**Read which step failed before reading anything into the job name.** A failure
in job setup means no project code ran, so it is never your diff — that is
infrastructure, and the right response is to say so and stop, not to change
code until something moves.

If `gh run view --log-failed` cannot reach the run's jobs, take the job id from
the checks output and fetch it directly:
`gh api repos/$CLAUDEFLOW_REPO/actions/jobs/<job-id>/logs`.

## 2. Reproduce it locally first

```
$CLAUDEFLOW_VERIFY
```

Your environment is up and is the same shape CI uses. A fix you have not seen
fail and then pass locally is a guess, and a guess costs a whole CI cycle to
disprove.

If it passes locally but fails in CI, that difference *is* the finding. Say so
rather than changing code until the symptom moves.

## 3. Fix the cause

Under the project's own rules — the same ones that applied when the code was
written. If the project requires tests first, a fix needs one too.

**Fix the failure, not the check.** Deleting an assertion, loosening a type,
skipping a test or suppressing a lint rule makes the check green and the
problem invisible, which is worse than red. If the check itself is wrong, say
so on the issue and stop.

## 4. Push and stop

Commit only the files you touched. Never `git add -A`, never `git stash`.

```
git push origin "$CLAUDEFLOW_BRANCH"
```

Then comment on issue `$CLAUDEFLOW_ISSUE` saying what failed and what you
changed, and stop. Do not watch the checks, do not merge, do not touch labels —
claudeflow is watching and will land it when it is green.

If you could not fix it, say what you found and what you think is wrong. Being
told "this is an infrastructure failure, not the diff" is worth more than
another attempt.
