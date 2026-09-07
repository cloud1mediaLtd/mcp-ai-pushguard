# PushGuard hardening backlog

Written 2026-09-07, from one release cycle in which several agents shared a
c1m-server checkout and PushGuard ran ~15 gates. Every item below is something
that actually happened, not something imagined.

## The class

Most of the day's lost time came from **a check reporting success while
verifying nothing**. Three independent instances, three different mechanisms:

- the architecture lane passed while `TestResponseDTOPurity` skipped;
- `consumers-check` printed `PASS — 0 consumer(s) current, 3 skipped` inside a
  worktree, because the siblings it resolves against are not there;
- an agent read `go vet ./... | head -3` printing nothing as success, and shipped
  a commit whose worker tests could not compile.

A gate that cannot fail is worse than no gate: it converts "unverified" into
"verified" and nobody looks again. The first two are fixed. The shape is not.

**Proposed rule, applicable beyond this repo: a check that verified zero things
must exit non-zero.** Reporting *what* was verified is what makes that
enforceable — `PASS` alone cannot distinguish "all three consumers current" from
"no consumers found".

---

## 1. Distinguish a gated COMMIT from a gated SNAPSHOT — P1

`make server-pre-push-q` enqueues without `--ref`, so it gates a working-tree
snapshot. That is right for iteration and wrong for a release, where the rule is
"CI green at that exact SHA". Nothing in the output says which happened, and a
snapshot whose parent is the tag candidate looks identical to the tag candidate
in every message the tool prints.

Make the job record the distinction and say it out loud: `gated commit <sha>`
versus `gated working-tree snapshot of <sha> (+N modified, +M untracked)`.

## 2. A `--release` mode — P1

Encode the release rule instead of relying on the operator to remember it:
refuse a dirty tree, require `--ref`, and fail if any check reported that it
skipped. One flag that means "this result is safe to tag on".

## 3. Warn on a duplicate job — P2

Two agents queued `make pre-push` at the same ref within a minute. At
concurrency 1 the second is ~8 minutes of queue for an answer the first already
produces. Detect an identical (repo, ref, command) job that is queued or
running, and say so at enqueue; let `--force` through for a deliberate re-run.

## 4. Generalise the sibling-repo problem — P1

`PUSHGUARD_ORIGIN_REPO` fixed `consumers-check`, but the underlying issue is
structural: **isolation removes context a check depends on**, and the check
cannot tell. A worktree under `~/.cache/pushguard/worktrees` has no siblings, no
`.env`, and no relationship to the developer's directory layout.

Let a job declare what it needs from the origin environment, and fail at enqueue
when it is unavailable — rather than each check discovering its own absence at
run time and, in the worst case, shrugging.

## 5. Reuse the Go build cache across jobs — P1, and it is also a disk bug

Go's build cache keys include the package's absolute directory. Every job gets a
fresh `worktrees/pg_<id>` path, so **no gate ever reuses the previous gate's
cache**: each run is a cold build of a large tree.

That is most of the ~8 minute gate time, and it is why the repo-local cache
reached 62 GB in a week and filled a 926 GB disk to 589 MiB free — 265,462 files
of which exactly one was older than seven days. Go trims entries unused for five
days; nothing here was ever that old, so trimming never applied.

A stable worktree path per repository, or a `GOCACHE` pinned outside the
worktree, fixes the time and the disk together.

## 6. Clean up after jobs — P2

Worktrees are removed; logs are not (`~/.cache/pushguard/logs` grows without
bound). A queued `make pre-push` also mints a `DB_SUFFIX` per job, and nothing
dropped the resulting database: 22 orphaned `c1m_test_platform_*` databases,
745 MB, one per suffix ever used. c1m-server now drops its own on success, but
the queue should not depend on the command being polite.

## 7. Surface daemon/binary version skew — P1

The daemon was rebuilt with a snapshot fix and kept running the old binary for
two hours. Nothing in `pushguard list` or `status` said so, and the stale binary
was silently excluding untracked files from snapshots — so a gate on a tree with
a new file tested code that could not compile **and reported it as the agent's
bug**. That cost one misdiagnosis and one wasted cycle.

Report the daemon's build alongside the client's, and warn when they differ.
A queue tool whose own state is invisible cannot be trusted to report on
anything else.

## 8. Make the REF column's meaning unambiguous — done, keep the lesson

The column preferred `CommitSHA` (the branch tip at enqueue) over `Ref` (the
commit actually gated), so a job deliberately gating an older SHA displayed as
the tip. An agent reasonably concluded `--ref` was broken and went looking for a
bug in the wrong place.

Fixed in `93749f8`. Worth keeping as the reason for item 1: **the tool's own
observability is load-bearing.** When it lies, every conclusion drawn downstream
is suspect, and the time is spent on the wrong problem.
