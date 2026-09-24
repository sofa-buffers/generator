---
name: cleanup
description: Return this repository to a clean starting position — fetch and fast-forward main from origin, prune stale remote refs, delete local branches whose work is already on main (squash-merges included), and clear leftover worktrees. Use before starting any new piece of work, and whenever a branch, a worktree or a stale checkout is suspected.
user-invocable: true
---

# /cleanup — a clean starting position

Working from a stale checkout is the failure this exists to prevent. It is not
hypothetical: a session once built a generator change on a `main` that was **30
commits and two days old**, reconstructed a superseded rule from the outdated code
it found there, and ran a full conformance suite that came back green — green
against a base that no longer existed, which proves nothing.

Run this **before** starting a piece of work, not only after finishing one.

## What it does, in order

### 1. Report before touching anything

Print the current branch, whether the tree is dirty, and how far `main` is behind
`origin/main`. If anything below has to be skipped, it is named in the summary —
never skipped silently.

```sh
git -C "$REPO" status --short
git -C "$REPO" log --oneline main..origin/main | wc -l
```

### 2. Fetch, with pruning

```sh
git -C "$REPO" fetch --prune --prune-tags origin
```

`--prune` is not optional: a branch deleted on the remote otherwise lingers as a
remote-tracking ref and shows up in every later listing as if it still existed.

### 3. Fast-forward main

```sh
git -C "$REPO" checkout main
git -C "$REPO" pull --ff-only origin main
```

**`--ff-only`, never a merge.** If it refuses, `main` has local commits that are
not on the remote — stop, report them, and let the user decide. Do not rebase or
reset them away.

If the working tree is dirty, **do not check out anything.** Report the dirty
paths and stop: uncommitted work is the one thing here that cannot be recovered.

### 4. Delete branches whose work is already on main

The subtlety that makes the obvious command wrong: this repository **squash-merges**,
so a merged branch shares no commit with `main` and `git branch --merged` does not
list it. Ask GitHub instead, which knows what a PR did:

```sh
gh pr list --repo <owner/repo> --state merged --limit 100 --json headRefName -q '.[].headRefName'
```

A local branch is deletable when **either**

* its name is in that list (its PR was merged, squashed or not), **or**
* `git branch --merged main` lists it (an ordinary merge or an ancestor).

Everything else stays. In particular **keep**:

* the branch currently checked out anywhere, including in a worktree;
* any branch with commits that are on no PR, or whose PR is open or closed-unmerged;
* any branch that is not an ancestor of `main` and has no merged PR — it is unpushed
  work until proven otherwise.

Delete with `git branch -d` (safe) and let it refuse; escalate to `-D` **only** for a
branch whose PR GitHub reports as `MERGED`, since a squash-merge makes `-d` refuse a
branch whose work is genuinely in. Say which ones needed `-D` and why.

### 5. Clear leftover worktrees

```sh
git -C "$REPO" worktree list --porcelain
git -C "$REPO" worktree prune
```

`prune` removes the bookkeeping for worktrees whose directory is already gone. A
worktree whose directory still exists is removed only when its branch was deleted in
step 4 **and** it has no uncommitted changes:

```sh
git -C "$REPO" -C <path> status --short     # must be empty
git -C "$REPO" worktree remove <path>
```

A worktree with changes is **reported, never removed**. The same goes for one whose
branch survived step 4.

### 6. Summarize

State plainly:

* what `main` moved from and to, and how many commits arrived;
* which branches were deleted, and which were kept with the reason;
* which worktrees were removed, and which were left alone with the reason;
* anything that blocked a step — a dirty tree, a non-fast-forward, a missing `gh`.

Then, if `main` moved at all, say so as a **warning**: any work in progress that was
based on the old `main` needs rebasing before it means anything.

## Boundaries

* **Never** `git reset --hard`, `git clean`, `git push --force`, or delete a remote
  branch. This command tidies the local checkout and nothing else.
* **Never** discard uncommitted changes, anywhere, for any reason. Stop and report.
* **Never** delete the branch that is checked out, here or in a worktree.
* Sibling clones outside this repository (a corelib checkout, a scratch directory)
  are **not** touched — they have their own lifecycles. Mention them if they look
  stale; do not act on them.
* If `gh` is unavailable or unauthenticated, do steps 1–3 and 5, skip the
  squash-merge detection in step 4, and say that it was skipped — an unverified
  branch is kept, never guessed at.
