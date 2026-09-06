# recall

Rust core for `entire recall`: checkpoint history as associative,
provenance-weighted memory. The Go side (`cmd/entire/cli/recall_*.go`) reads
Entire's checkpoint store and renders; every ranking decision lives here.
Design and rationale: `docs/recall/checkpoint-1-architecture.md`.

## Build

```sh
cd recall && cargo build --release      # binary at target/release/recall
cargo test                              # reference tests + additions
cargo run --release --example bench     # agreement() over the 840 labelled pairs
```

`entire recall` finds the binary via `$ENTIRE_RECALL_BIN`, then `recall` on
`$PATH`, then `recall/target/{release,debug}/recall` inside the repo.

## Demo checklist (read before presenting)

Ingest asks the code graph for each changed file's blast radius. The graph
caches its index **per HEAD commit**: the first query after HEAD moves rebuilds
the index (about 90 s in this repo); every later query is ~1.5 s.

1. Make the last commit you intend to show.
2. Warm the cache once:
   ```sh
   entire graph index --repo .
   ```
3. Build the memory:
   ```sh
   entire recall ingest            # ~1.5 s per impact query, capped at 40
   ```
4. Ask questions:
   ```sh
   entire recall why did we drop the retry wrapper
   entire recall --json what is unfinished in dispatch
   ```

**Do not move HEAD during the demo** (no commits, amends, rebases, or
checkouts): the next ingest would pay the full re-index again. If you must
commit, run step 2 again before step 3. `entire recall ingest --no-graph`
skips the graph entirely (fast, but the isolation check can never fire).

`.entire/recall/` is derived state, gitignored, and rebuilt from scratch on
every ingest.

## Graph reach: how edges are found

The first `+` line of a new file is a package or import line and resolves to no
symbol, so impact-by-diff-line yields nothing. Ingest instead streams
`entire graph symbols` once, picks the first two code symbols each touched
file defines, runs `entire graph impact --head` on each, and unions the files
of their callers, type consumers and data flows. Files with no code symbols
(docs, JSON) get no edges. Nothing is fabricated: no graph, no edges.
