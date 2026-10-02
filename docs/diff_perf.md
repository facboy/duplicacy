# Where `diff` spends its time

Review of `duplicacy diff`, in the style of `snapshot_perf.md`, `copy_perf.md`,
`prune_perf.md`, `check_perf.md`, `restore_perf.md`, `backup_perf.md`,
`history_perf.md` and `init_perf.md`. This one is a measurement rather than the
code review `history_perf.md` was: the command compares a fixed pair of
revisions, so its metadata cost is bounded by that pair, but the `diff <file>`
form runs a third-party line diff whose cost is quadratic in the length of the
file, the only cost in the command that grows with the payload rather than with
the revision count. Candidate #1 is recorded
as not going to be implemented; candidates #2, #3 and #4 are the three shapes
already fixed in `history`, and each is bounded to at most two revisions here.

## Summary

`diff` compares two revisions of a snapshot, or two revisions of one file. Four
properties cost it work:

1. the `diff <file>` form runs `github.com/aryann/difflib` over both files, whose
   `longestCommonSubsequenceMatrix` allocates one `int` per pair of lines — O(n·m)
   memory and time — and the path also holds both whole files in memory
   (candidate #1) — **Not going to be implemented**;
2. with two revisions and no file argument, both sides expand the chunk-hash
   sequence, which the comparison never reads (candidate #2) — the defect
   `snapshot_perf.md` fix #8 fixed for `list -files`;
3. an explicit `-r` pays the per-revision existence check (candidate #3) — the
   `snapshot_perf.md` fix #3 shape;
4. the two snapshots and their sequences are read one after the other on a
   one-thread operator (candidate #4) — the `snapshot_perf.md` fix #4 shape;
   `diff` has no `-threads` flag.

Candidate #1 is the only one that is not a shape already fixed elsewhere, and the
only one whose cost grows with the payload rather than with the revision count.
Measured on ext4, `diff -r A -r B f.txt` on a 16,000-line file takes 1.8-2.7 s
and 2.1 GB, against 0.11 s for the same revisions without the file argument, and
the resident set tracks `lines × lines × 8` bytes exactly. It is recorded and not
implemented because the fix is a different diff algorithm rather than a call that
can be dropped, and it changes the output for some inputs. Candidates #2, #3 and
#4 are each a small change, but `diff` reads at most two revisions, so on a cloud
storage the whole of candidate #3 is two round trips and the whole of candidate
#4 is one overlap; they are recorded and not implemented here.

## The call path

`diff` (`duplicacy/duplicacy_main.go:1072`) parses the flags, creates the storage
with one thread (`:1087`) — the command has no `-threads` flag (`:1821-1856`) —
and calls `BackupManager.SnapshotManager.Diff` (`:1120`).

`Diff` (`src/duplicacy_snapshotmanager.go:1869`) then does:

```go
manager.CreateChunkOperator(false, false, 1, false)                 // :1875  one thread
defer manager.stopChunkOperator()

if len(revisions) <= 1 {
    rightSnapshot = CreateEmptySnapshot(snapshotID)                 // :1888  the on-disk side
    go rightSnapshot.ListLocalFiles(top, ...)                       // :1890  the local walk
    ... collect rightSnapshotFiles ...
} else {
    rightSnapshot = manager.DownloadSnapshot(snapshotID, revisions[1])  // :1902  listed=false
    manager.DownloadSnapshotSequences(rightSnapshot)                    // :1903  chunks + lengths
}

if len(revisions) < 1 {
    leftSnapshot = manager.downloadLatestSnapshot(snapshotID)       // :1908  already listed
} else {
    leftSnapshot = manager.DownloadSnapshot(snapshotID, revisions[0])   // :1914  listed=false
}
manager.DownloadSnapshotSequences(leftSnapshot)                     // :1917  chunks + lengths

if len(filePath) > 0 {
    RetrieveFile(leftSnapshot, FindFile(leftSnapshot, filePath, false))  // :1921
    RetrieveFile(rightSnapshot, ...) or ioutil.ReadFile(...)             // :1931 / :1940
    for _, diff := range difflib.Diff(leftLines, rightLines) { ... }     // :1956
    return true
}

leftSnapshot.ListRemoteFiles(manager.config, manager.chunkOperator, ...)   // :1992
rightSnapshot.ListRemoteFiles(manager.config, manager.chunkOperator, ...)  // :1999
... merge the two sorted lists, comparing Hash ...                        // :2074
```

`DownloadSnapshot` (`:234`) delegates to `downloadSnapshot` with `listed=false`,
which runs `GetFileInfo` before the download (`:253-265`), so the two
`DownloadSnapshot` calls above each pay an existence check. The plural
`DownloadSnapshotSequences` (`:508`) is the pair of `"chunks"` and `"lengths"`
calls, and the singular `DownloadSnapshotSequence(snapshot, "lengths")` (`:478`)
is one of them. `ListRemoteFiles` (`src/duplicacy_snapshot.go:107`) validates each
entry against `snapshot.ChunkLengths` (`:189`) and never reads `ChunkHashes`;
`RetrieveFile` (`src/duplicacy_snapshotmanager.go:1735`) is the only reader,
indexing `snapshot.ChunkHashes[i]` at `:1761`.

## The `diff <file>` line diff is quadratic — candidate #1 — **Not going to be implemented**

`Diff` collects both files whole:

```go
var leftFile []byte
manager.RetrieveFile(leftSnapshot, ..., func(content []byte) { leftFile = append(leftFile, content...) })   // :1922
var rightFile []byte
manager.RetrieveFile(rightSnapshot, ..., func(content []byte) { rightFile = append(rightFile, content...) })  // :1932
leftLines := strings.Split(string(leftFile), "\n")     // :1947
rightLines := strings.Split(string(rightFile), "\n")   // :1948
for _, diff := range difflib.Diff(leftLines, rightLines) { ... }   // :1956
```

`difflib.Diff` trims the common head and tail, and then hands what is left to
`compute`, whose first act is `longestCommonSubsequenceMatrix` — an
`intMatrix(len(seq1)+1, len(seq2)+1)`, one Go `int` per pair of surviving lines
(`github.com/aryann/difflib` `difflib.go:142-155`). The table is O(n·m) both in
time and in memory, and it has to exist before a single diff line is printed.

Measured on ext4, one file per revision whose content is entirely different
between the two, `/usr/bin/time -f "%e %M"`, best of three runs:

| lines in each file | `diff -r A -r B <file>` | maxRSS | `diff -r A -r B` |
| --- | --- | --- | --- |
| 2,000 | 0.14 s | 84 MB | 0.11 s |
| 4,000 | 0.23 s | 170 MB | 0.11 s |
| 8,000 | 0.55 s | 557 MB | 0.11 s |
| 16,000 | 1.81 s | 2,100 MB | 0.11 s |

The resident set is the matrix plus a little: `n² × 8` bytes is 32, 128, 512 and
2,048 MB for the four rows, and the measured figures are those plus a process
baseline of 40-50 MB. That figure is the most repeatable of the two, because the
allocation is deterministic: the three runs at 16,000 lines agree to within
0.1%, where the small sizes vary by several percent because the table is not what
dominates. The time has more spread still — 1.8-2.7 s at 16,000 lines across
fixtures — but the quadrupling of the table size at each doubling of the line
count is visible in both, and it is the memory that is the hard limit: the
command is allocating, not waiting. The same command without the file argument
stays flat, because the whole-snapshot form compares hashes and never runs the
line diff.

`/usr/bin/time -v` at 16,000 lines separates the two: 2.31 s elapsed against 1.25
s user and 0.97 s system time, 96% of the wall clock on the CPU. The system time
is the page faults for the 2 GB the matrix faults in, not I/O.

The cost follows the number of lines that differ, not the size of the file. On the
same 16,000-line pair, `/usr/bin/time`:

| the two 16,000-line revisions differ in | time | maxRSS |
| --- | --- | --- |
| one line | 0.14 s | 79 MB |
| the first 8,000 lines, then all different | 0.76 s | 559 MB |
| every line | 2.28 s | 2,100 MB |

The common head is trimmed, which quarters the table for the middle row: 8,000
surviving lines a side is a quarter the area of 16,000, and 512 MB against
2,048 MB is what the memory column shows. The two halves that do differ still
cost the product of their lengths. There is no threshold at which the command
degrades gracefully — it allocates the table and then prints, so peak memory is
reached before the first line of output.

**Not going to be implemented.** Three ways to remove the term, none of them an
edit to this command:

- replace `difflib` with a linear-space Myers diff, or a histogram/patience diff.
  That is a new dependency or a hand-written algorithm, and the line anchoring it
  produces is not identical to the LCS one, so the printed diff changes for some
  inputs — a behaviour change beyond a performance fix.
- stream the comparison instead of buffering both files. That removes the two
  `[]byte` copies and the two `strings.Split` slices, not the table, so on its own
  it does not change the order of growth.
- bound the output and print only that the two files differ above some line
  count. That is a deliberate loss of the command's output on exactly the inputs
  where it is most useful.

Recorded with the measurements so a future change to the diff algorithm has a
justification; the case is a human diffing a multi-thousand-line text file across
revisions, which does not scale with the repository the way the round-trip costs
do.

## The whole-snapshot form expands the chunk-hash sequence it never reads — candidate #2

With two revisions and no file argument, `Diff` calls
`DownloadSnapshotSequences` on both sides (`:1903`, `:1917`), and the comparison
that follows reads only what `ListRemoteFiles` needs: `Entry.check` validates each
entry against `snapshot.ChunkLengths` (`src/duplicacy_snapshot.go:189`), the merge
compares `Hash`, `Size` and `Compare` (`src/duplicacy_snapshotmanager.go:2054`,
`:2074`), and the printed line is `Entry.String` (`src/duplicacy_entry.go:521`).
None of them touches `snapshot.ChunkHashes`. This is `snapshot_perf.md` fix #8,
except that it runs twice — once per side — instead of once per revision.

The per-file branch is not affected: `RetrieveFile` indexes
`snapshot.ChunkHashes[i]` at `:1761`, so `diff <file>` genuinely needs both
sequences.

Measured with `strace -f -e trace=openat`, counting opens under `chunks/` on a
local storage:

```
diff -r 1 -r 5                    6
list -files -r 1 -r 5             4
diff -r 1 -r 5 bigfile.txt        8
```

The two extra opens for the no-file form are the chunk-hash chunk of each side —
one metadata chunk per revision, one round trip on cloud storage and a
`newfstatat` plus `openat` plus the cache write on a local mount, the same
per-chunk saving fix #8 measured for `list -files` (`docs/snapshot_perf.md:311-325`).

The fix is the one `list -files` took, applied to the `filePath == ""` case:
`DownloadSnapshotSequence(snapshot, "lengths")` on each side in place of
`DownloadSnapshotSequences`. The two calls are at the top of the same function, so
the change is two lines and a condition on `filePath`. Recorded; the absolute
saving is two metadata chunks.

## Explicit `-r` pays the existence check — candidate #3

Both `DownloadSnapshot` calls pass `listed=false` (`:1902`, `:1914`), so
`downloadSnapshot` runs `GetFileInfo` before `downloadFile` (`:253-265`). Neither
side was enumerated by a directory listing, so this is not the pure deletion that
`snapshot_perf.md` fix #3 was for `list`: the check is also what reports a named
revision that does not exist as `SNAPSHOT_NOT_EXIST` rather than as a failed
download, and what keeps a stale snapshot cache entry from being believed.

Measured with `strace -f -e trace=newfstatat`, counting calls under
`snapshots/<id>/` on the storage:

```
diff -r 1 -r 5    2
diff -r 5         1
diff              0
```

`diff` with no `-r` pays nothing, because its left side comes from
`downloadLatestSnapshot` (`:1908`), which lists the directory itself and passes
`listed=true` (`:696`). The fix is the one `history` adopted: list the snapshot
directory once and set `listed` when every named revision is present. That
replaces two `GetFileInfo` calls with one listing for `diff -r A -r B`, and one
call with one listing for `diff -r B` — a listing being one round trip against
the one it replaces, so the two-revision case is the only one that gains, and by
a single round trip. Recorded; the smallest of the four.

## Both revisions are read on one thread, one after the other — candidate #4

`Diff` creates its operator with one thread (`:1875`) and calls
`manager.DownloadSnapshot` twice in sequence (`:1902`, `:1914`), then expands the
sequences in sequence (`:1903`, `:1917`). There is no `-threads` flag on the
command, and the storage is created with one thread (`duplicacy_main.go:1087`).
Each snapshot download and each metadata chunk is a round trip on cloud storage,
and on a slow mount each is syscalls; `list` overlaps the first of those with
`downloadSnapshots` (`:880`), and `check`, `restore`, `prune` and `backup` have
since adopted the same shape.

The bound here is two revisions, so the gain is one overlap rather than the
linear-in-revisions gain the other commands get. A fix adds the flag as `history`
did and reuses `downloadSnapshots`. Recorded; not implemented, because a
`-threads` flag that can only ever overlap two units of work is a flag whose
default and maximum are the same.

## Candidate fixes

### Smaller items

- **The on-disk walk is `backup`'s walk.** When the right side is the working
  tree (`diff` and `diff -r N` with no file, `:1885-1900`), `ListLocalFiles`
  (`src/duplicacy_snapshot.go:66`) runs `ListEntries` over the repository, which
  is one `getdents64` loop, one `newfstatat` per entry and one `listxattr` per
  entry (`src/duplicacy_entry.go:708`, `:774`, `src/duplicacy_utils_others.go:53`).
  Measured on a 20,000-file fixture: `diff -r 2` issues 20,000 `listxattr` and a
  matching `newfstatat` per entry, while `diff -r 1 -r 2` — both revisions
  remote — issues none, because no side is a local path. That is the walk
  `backup_perf.md` records as examined and retained, and it is not specific to
  `diff`.
- **`-hash` hashes the working tree, not the snapshots.** On the same fixture,
  `diff -r 2 -hash` took 2.80 s against 0.26 s for `diff -r 2`: `ComputeFileHash`
  reads and hashes every file for which the revision comparison needs a hash
  (`:2064-2066`). That is what the flag promises, as in `history`.
- **The merge itself is linear.** Once both sides are in memory, the two-way
  merge over the sorted entry lists (`:2027-2086`) is one pass, and the
  `maxSize` pre-pass (`:2009-2022`) is two more; neither depends on the file
  contents.
- **The chunk-cache and snapshot-cache decisions are already handled.**
  `prune_perf.md` (option 2) removed the per-write `fsync` from the chunk cache
  and `check_perf.md` made the cache decision filesystem-based, so `diff` — which
  creates its operator with `rewriteChunks` false, like `list` — already benefits.
- **`diff <file>` with a local right side never expands the right sequences.**
  `CreateEmptySnapshot` (`:1888`) has no sequences to expand, and the file is read
  with `ioutil.ReadFile` (`:1940`). Only the left side pays metadata.

### Deliberately not pursued

- **A chunk or revision index.** As in `snapshot_perf.md` (candidate #7),
  `copy_perf.md` and `prune_perf.md`, this would change the storage format on
  disk.
- **A bounded or streaming line diff.** Covered under candidate #1; recorded as
  not going to be implemented rather than left open.

## How to confirm on a given setup

- `duplicacy -d diff <file>` sets DEBUG logging
  (`duplicacy/duplicacy_main.go:142-148`); `-v` adds TRACE. The
  `DIFF_PARAMETERS` line names the two revisions, each `Downloaded file
  snapshots/<id>/<rev>` is one snapshot download, and `CHUNK_DOWNLOAD` /
  `CHUNK_CACHE` count the metadata chunks behind them.
- Candidate #1: `/usr/bin/time -f "%e %M" duplicacy diff -r A -r B <file>` on a
  large file, and read the maximum resident set; it tracks `lines² × 8` bytes.
  `duplicacy -print-memory-usage diff ...`
  (`duplicacy/duplicacy_main.go:150-152`) prints the live heap while it runs, and
  the peak is reached before the first diff line is printed. Compare with `diff -r
  A -r B` without the file argument, which stays flat. A file pair that differs
  everywhere is the worst case; one that shares a long head or tail is trimmed
  before the table is built.
- Candidate #2: `strace -f -e trace=openat duplicacy diff -r A -r B` and count the
  opens under `chunks/` (6 on the fixture), against `duplicacy list -files -r A
  -r B` (4). The difference is the chunk sequence of each side, one chunk per
  revision. The per-file form reads both sequences by design and its count is
  higher for that reason.
- Candidate #3: `strace -f -e trace=newfstatat duplicacy diff -r A -r B` and
  count the calls under `snapshots/<id>/`: two for two named revisions, one for a
  single named revision, none when both revisions were resolved from a listing.
- Candidate #4: not observable on a local fixture, where the whole run is a few
  syscalls. On a storage where a read is a round trip, compare `diff -r A -r B`
  with `list -r A -r B`, which already overlaps its two downloads; the gap is the
  serial pair candidate #4 would remove.
