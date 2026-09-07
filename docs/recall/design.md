# entire recall — design and rationale

`entire recall` turns Entire checkpoint history into associative,
provenance-weighted memory: a developer or a fresh agent session asks "what did
we decide, and why" and gets ranked, trust-scored answers with the commit
evidence behind each one.

## The problem

Git records what changed; Entire checkpoints preserve why. But preserved context
is an archive, not a memory. Searching it returns transcripts, and a transcript
does not distinguish a decision from an agent narrating itself, a claim the
commit record backs from one the code contradicts.

That last category is the dangerous one. An agent session can state that a
change is isolated to one package while the change reaches three others, or name
a function that does not exist in the diff. Those statements sit in checkpoint
history looking exactly as authoritative as everything else, and a resuming
developer — or a resuming agent — will act on them.

`entire recall` therefore ranks checkpoint context by how much it can be
trusted, not by how well it matches the query string.

Three things the checkpoint store provides make this possible, and nothing in a
generic index over `git log` would:

1. **The prompt/transcript split.** A user prompt is a stated intent; an
   assistant turn is an unverified claim. Entire's session capture carries that
   distinction, and it is the basis of the provenance tiers.
2. **The commit↔context link.** The `Entire-Checkpoint` trailer binds a claim to
   the commit it was made about, which is what allows checking a claim against
   what the code actually did rather than against other text.
3. **`entire graph`.** Structural reach turns "the session said this change is
   isolated" from an opinion into a testable assertion.

## Architecture

Two layers, split so that no ranking logic lives in the CLI.

```
 entire/checkpoints/v1 + Entire-Checkpoint trailers
                 │
        Go shim (cmd/entire/cli/recall_cmd.go, recall_collect.go)
        ├─ commits with trailers  → sha, subject, files, unified-0 diff
        ├─ checkpoint store       → transcript → user / assistant turns
        └─ JSON on stdin ────────────────────────────────┐
                                                         ▼
                                    Rust crate  recall/  (fluctlightdb)
                                    ├─ ingest:   checkpoints → engrams
                                    ├─ graph:    entire graph impact → file-edge map
                                    └─ activate: question → ranked hits (JSON out)
                                                         │
        Go shim renders text  ◄──────────────────────────┘
```

The Go side knows how to read Entire's own storage (the checkpoint store, the
trailer parser, the transcript condenser) and how to render. It makes no scoring
decisions, so the benchmarked algorithm is ported verbatim rather than
re-derived. It is registered as an experimental, task-driven command.

### Ingest mapping

| checkpoint element  | stored as                                    | salience |
| ------------------- | -------------------------------------------- | -------- |
| user prompt         | Episode, `ProvenanceKind::UserExplicit`      | 0.85     |
| assistant turn      | Episode, `ProvenanceKind::ChatAssertion`     | 0.45     |
| commit message + SHA| Episode, `ProvenanceKind::LedgerVerified`    | 0.90     |
| same-session steps  | co-activation edges                          | —        |
| changed files       | edges to file-node engrams                   | —        |

Context string: `checkpoint:<id> commit:<sha> role:<role>`.

### Ranking

Ranking never sorts on raw activation. For each recalled engram:

1. classify provenance into a `SourceKind`;
2. run `agreement()` against the commit that engram sits on, yielding
   `Corroborated` | `Neutral` | `Contradicted` | `Unverifiable`;
3. corroboration adds an independent verified evidence source; contradiction
   adds a low-reliability source and damps confidence;
4. fuse via noisy-OR into a calibrated confidence, then
   `score = activation × activation_multiplier(confidence)`;
5. sort and dedupe.

`agreement()` runs six checks in order: scope contradiction against graph reach;
file contradiction (the claim names a path the commit never touched); action
polarity flip against the commit's own polarity; identifier anchoring (a
code-shaped token absent from the diff vocabulary); lexical agreement with the
commit subject; and 4-gram cosine against the diff as a corroboration signal.

The consequence is that the highest-relevance hit is not always the top hit. A
claim the code refutes is demoted and labelled, with the refuting commit shown
alongside.

## Why FluctlightDB

`recall/` depends on [FluctlightDB](https://github.com/voxmastery/FluctlightDB)
(MIT/Apache-2.0), an embedded memory engine, as an unmodified git dependency.

- **Spreading activation, not vector search.** The question is answered by
  association through a graph of engrams (prompts, assertions, commits, files),
  so a paraphrased question still reaches the commit it is about via the turns
  that surrounded it. No embedding model, no network, deterministic.
- **Provenance is a first-class field.** `Episode.provenance` distinguishes a
  user's stated intent from an assistant's assertion from the ledger record, and
  `confidence::recall_confidence` fuses independent evidence via noisy-OR. That
  is precisely the trust model this feature needs, already built.
- **Durable on disk, reopenable read-only.** `open` / `checkpoint` /
  `open_readonly` give an on-disk brain under `.entire/recall/` that survives
  process restarts, so ingest and query can be separate invocations.
- **Cheap.** Roughly 145 µs per claim/commit verdict.

The dependency is pinned to a git revision, so a break there is a break here.
Nothing is vendored.

## Privacy boundary

The feature is designed to stay useful on sensitive repositories, and the rules
below are structural rather than advisory. `recall/README.md` documents the
user-facing side in full; the load-bearing points are:

- **Nothing new leaves the machine.** FluctlightDB is embedded, the brain
  directory is local and gitignored, and `entire graph` runs locally.
  `recall/scripts/verify-offline.sh` traces ingest and activate under `strace`
  and fails on a single socket call.
- **No checkpoint is ever silently dropped.** When a transcript cannot be read,
  the checkpoint is still ingested from its commit metadata with
  `unavailable: ["session"]`, so the ledger engram survives and a sensitive
  repository still gets commit-level recall.
- **`Unverifiable` is distinct from `Neutral`.** `Neutral` means the checks ran
  and found nothing; `Unverifiable` means a check could not run because its
  input was unavailable. Collapsing the two would present an incomplete check as
  a completed one. Two of the six checks treat *absence from the diff* as
  evidence against a claim, so under redaction they would otherwise manufacture
  confident `Contradicted` verdicts. An unverifiable result short-circuits
  rather than falling through to a later corroboration.
- **Availability is declared, never inferred.** The shim states which fields are
  missing on the wire type and whether the graph was available. An inline
  redaction marker adds the partial marker and the confidence cap but never
  changes a verdict — otherwise the benchmark's 17 marker-carrying pairs, and
  its empty graph, would move the numbers.
- **Confidence is capped, not merely lowered.** Any non-ledger hit drawn from a
  partial checkpoint is clamped to 0.50 *after* all other scoring — above the
  0.30/0.35 baselines, below a corroborated hit near 0.96 — so partial context
  can rank but can never outrank fully verified context.
- **Coverage is structural.** `activate` returns
  `{"coverage": ..., "hits": [...]}` rather than a bare array, and text output
  prints the context line before any result. A brain with no coverage record
  refuses to activate rather than defaulting to "complete".
- **`--no-transcripts`** never opens session content at all, marking every
  checkpoint `unavailable: ["session"]` by policy.

The unredacted path is byte-identical: verdict routing changes only when an
unavailable field or `graph_available: false` is present.

## Evaluation

The published benchmark for message–code inconsistency is CodeFuse-CommitEval
(arXiv 2511.19875). Its dataset is unavailable — the LFS objects 404 — so the
fixture here is reconstructed from the paper's published taxonomy: 300 real
commits from this repository's history, mutated under four of its seven rules
(operation-type, file-path, function-name, component mismatch), yielding 840
labelled claim/commit pairs in `recall/bench/bench_840.json`.

`agreement()` as shipped, over those pairs:

| metric      | value       |
| ----------- | ----------- |
| Precision   | 0.869       |
| Recall      | 0.270       |
| Specificity | 0.927       |
| Latency     | ~145 µs/pair|

Reproduce with `cargo run --release --example bench`.

Against the paper's average across six LLMs (P 0.803, R 0.860, Spec 0.638) this
matches on precision, beats every model tested on specificity, and runs four
orders of magnitude cheaper, while being well behind on recall. The trade is
deliberate: every failure is a false negative on corroboration — saying
"unverified" where "corroborated" was available. For a trust signal a developer
relies on while resuming unfamiliar work, a tool that is right 87% of the time
when it speaks and silent otherwise beats one that flags more and is wrong more.

An IDF-anchoring ablation raised recall to 0.81 but collapsed specificity to
0.51, and was rejected: a reviewer tool that false-alarms on half of all healthy
commits gets uninstalled.

### Data provenance of the fixture

`recall/bench/bench_840.json` is generated from this repository's public commit
history. GitHub secret scanning flags an AWS Access Key ID inside it. That
string is Entire's own fixture for testing secret redaction, present across many
commits in this repository, and is not a live credential.

## Known limitations

- **Recall is 0.270.** The tool is a high-precision screen, not a comprehensive
  detector, and should be described that way to users.
- **Thresholds are repo-calibrated; the structural check is not.** Evaluated
  across six repositories in four languages (4,961 labelled pairs generated by
  the same harness; only the 840-pair set ships here), the thresholded checks do
  not transfer — specificity ranges from 0.23 to 0.72 depending on commit style:

  | repo          | language    | pairs | P     | R     | Spec  |
  | ------------- | ----------- | ----- | ----- | ----- | ----- |
  | entireio/cli  | Go          | 840   | 0.800 | 0.624 | 0.720 |
  | fastapi       | Python      | 889   | 0.827 | 0.868 | 0.643 |
  | fzf           | Go/shell    | 800   | 0.720 | 0.776 | 0.497 |
  | express       | JavaScript  | 805   | 0.672 | 0.850 | 0.303 |
  | bat           | Rust        | 812   | 0.670 | 0.846 | 0.290 |
  | requests      | Python      | 815   | 0.664 | 0.885 | 0.230 |

  The identifier anchor, which is structural rather than tuned, does transfer:
  measured alone it holds specificity between 0.847 and 0.950 on every
  repository, with precision never below 0.67. Per-repository calibration of the
  n-gram and diff-miss thresholds is the first item of production work before
  this is safe to run outside a tuned repo.
- **The semantic lane is dark.** Engrams are ingested without embedding vectors,
  so activation runs on the lexical index alone and paraphrase reads as Neutral
  rather than Corroborated. Populating `semantic_vector` with a local embedder is
  the highest-value next step.
- **Real transcripts are noisy.** A pasted docs link no longer reads as an
  untouched repo path (pinned by `url_in_claim_is_not_a_file_path`), but prose
  containing a slash can still be misread, and long single-turn prompts degrade
  ranking: a 2.5 KB brief matches almost any question by term count and one
  false path token then contradicts the whole turn. The fix is per-sentence
  claims rather than per-turn, which is ingest work rather than ranking work.
- **Graph cache invalidation.** Moving HEAD invalidates the graph cache and
  re-indexing costs ~90 s, which makes the first ingest after a commit slow. See
  `recall/README.md` for the warming procedure.

**Next steps, in order:** populate semantic vectors to close the paraphrase gap;
per-repo threshold calibration; incremental ingest so recall stays fast as
history grows; and surface `recall` output directly inside an agent session
rather than as a separate command.
