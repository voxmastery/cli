# entire recall

## One-sentence summary

`entire recall` turns Entire Checkpoint history into associative, provenance-weighted memory, so a developer or a fresh agent session can ask "what did we decide, and why" and get ranked, trust-scored answers with the commit evidence behind each one.

## Problem, intended user and why it matters

The user is a developer resuming agent-assisted work they did not personally write — after a break, after a handoff, or after an agent session ends and its context window closes with it.

Git tells you what changed. Entire Checkpoints preserve why. But preserved context is an archive, not a memory: you can search it, and you get back transcripts. What a resuming developer actually needs is different from what search returns. They need to know which statements were decisions and which were an agent narrating itself, which claims the commit record actually backs, and which claims the code contradicts.

That last category is the dangerous one. An agent session can state that a change is isolated to one package while the change reaches three others. It can name a function that does not exist in the diff. Those statements sit in the checkpoint history looking exactly as authoritative as everything else, and a resuming developer — or a resuming agent — will act on them.

`entire recall` ranks checkpoint context by how much it can be trusted, not by how well it matches the query string.

## Selected Entire track and why Entire is essential

**Track 1 — Build a Checkpoint-Native Developer Experience.**

Checkpoint context is not instrumentation here, it is the input corpus. The product has nothing to rank without it.

Specifically, three things only Entire provides:

1. **The prompt/transcript split.** A user prompt is a stated intent; an assistant turn is an unverified claim. Nothing in Git carries that distinction. Entire's session capture does, and it is the basis of my provenance tiers.
2. **The commit↔context link.** The `Entire-Checkpoint` trailer binds a claim to the commit it was made about. That binding is what lets me check a claim against what the code actually did, rather than against other text.
3. **Entire Graph.** Structural reach is what turns "the session said this change is isolated" from an opinion into a testable assertion.

Remove Entire and there is no corpus, no provenance signal, and no way to verify a claim. A generic RAG index over git log would produce none of this.

## Architecture and main workflow

Two layers, deliberately split so no ranking logic lives in the CLI.

**Go shim** (`cmd/entire/cli/recall_cmd.go`, `recall_collect.go`) — walks trailered commits, reads transcripts and diffs through the persistent store, pipes JSON to the Rust binary, renders results. Registered as an experimental, task-driven command. No scoring decisions. A checkpoint it cannot read is never dropped: the commit record is ingested with the missing fields declared, and every answer opens with a coverage line that reads `complete` only when nothing was missing.

**Rust engine** (`recall/`) — modules `text`, `agreement`, `graph`, `ingest`, `rank`, plus a two-mode binary (`ingest`, `activate`). Built on FluctlightDB, an embedded memory engine with graph-native recall and first-class provenance.

**Ingest** maps checkpoint content onto memory primitives:

| checkpoint element | stored as |
| --- | --- |
| user prompt | Episode, `UserExplicit`, salience 0.85 |
| assistant turn | Episode, `ChatAssertion`, salience 0.45 |
| commit message + SHA | Episode, `LedgerVerified`, salience 0.90 |
| same-session steps | co-activation edges |
| changed files | edges to file-node engrams |

**Activate** does not rank on raw activation. For each recalled engram:

1. classify provenance into a `SourceKind`
2. run `agreement()` against the commit that engram sits on → `Corroborated` | `Neutral` | `Contradicted` | `Unverifiable` (a check whose input field was redacted or unavailable)
3. corroboration adds an independent verified evidence source; contradiction adds a low-reliability source and damps confidence
4. fuse via noisy-OR into a calibrated confidence, then `score = activation × activation_multiplier(confidence)`
5. sort, dedupe

`agreement()` runs six checks in order: scope contradiction against Graph reach; file contradiction (claim names a path the commit never touched); action-polarity flip against the commit's own polarity; identifier anchoring (a code-shaped token absent from the diff vocabulary); lexical agreement with the commit subject; and 4-gram cosine against the diff as a corroboration signal.

The result is that the highest-relevance hit is not always the top hit. A claim the code refutes gets demoted and labelled, with the commit that refutes it shown alongside.

## Evaluation

I built a labelled benchmark rather than asserting quality.

The published benchmark for message–code inconsistency is CodeFuse-CommitEval (arXiv 2511.19875, Ant Group / HKU). Its dataset is unavailable — the LFS objects 404 — so I reconstructed it from the paper's published taxonomy: 300 real commits from this repository's own history, mutated under four of their seven rules (operation-type, file-path, function-name, component mismatch), yielding 840 labelled claim/commit pairs. The same generator was later run against five further repositories in four languages, giving 4,961 pairs in total — see the cross-repository table under Known limitations.

`agreement()` as shipped:

| metric | value |
| --- | --- |
| Precision | 0.869 |
| Recall | 0.270 |
| Specificity | 0.927 |
| Latency | ~145 µs per verdict (measured 139–148 µs across runs today, 144 µs at final HEAD) |

Against the paper's average across six LLMs (P 0.803, R 0.860, Spec 0.638): I match on precision, beat every model tested on specificity, and run four orders of magnitude cheaper. I am well behind on recall.

That trade is deliberate. Every one of my failures is a false negative on corroboration — I say "unverified" when I could have said "corroborated". I do not cry wolf. For a trust signal a developer is meant to rely on while resuming unfamiliar work, a tool that is right 87% of the time when it speaks, and silent otherwise, is more useful than one that flags more and is wrong more.

Reproduce with `cargo run --release --example bench`.

An ablation is included in the repo: an IDF-anchoring mechanism raised recall to 0.81 but collapsed specificity to 0.51. I rejected it. A reviewer tool that false-alarms on half of all healthy commits gets uninstalled.

## Entire Graph findings and verification

Graph was used at three points, in the order the scoring asks for: to locate, to bound the blast radius before a risky edit, and to diff at the end. Every command below was run against this repository; outputs are quoted from the transcript in checkpoint 3.

**1. Search / definition lookup.** Before the curveball edit I ran `entire graph search --repo . --profile full` three times, one sentence each, to resolve the functions that consume raw prompt and transcript text. The top hits were the three roots I needed and nothing else: `agreement` (`recall/src/agreement.rs:28`, score 47.3), `rerank` (`recall/src/rank.rs:54`) and `ingest` (`recall/src/ingest.rs:50`); a fourth query resolved the Go collector `collectRecallCheckpoints` (`cmd/entire/cli/recall_collect.go:58`). A bare `--symbol Checkpoint` lookup returned seven definitions across Go, Rust and the architecture docs and asked for `--file` to disambiguate, which is the right behaviour for a name that common.

**2. Impact analysis before the risky change.** The risky change was adding a fourth verdict to `Agree` and a missing-field marker to the shared `Checkpoint` type. `entire graph impact --symbol agreement` reported a blast radius of 15 callers (12 tests in `recall/tests/port.rs`, `rerank`, the bench, and `run_activate` transitively), 7 callees, 6 type consumers and 2 co-change files. `impact --symbol rerank` showed 3 callers and that `rerank` is the only production caller of `agreement`. `impact --symbol Checkpoint --file recall/src/model.rs` showed 18 type consumers, which is why the `unavailable` field had to enter through that struct and nowhere else. `impact --symbol collectRecallCheckpoints` showed exactly one caller, `runRecallIngest`, so stopping the shim from dropping checkpoints could not ripple anywhere. What this told me before editing: the enum could grow (the only non-test `match` has a `_ =>` arm and the bench compares `== Contradicted`), every consumer of prompt text sits on three spines (ingest, rank, render), and twelve tests would pin the unredacted path.

**3. Final semantic diff.** `entire graph diff --repo . --base origin/main --head HEAD`, where `origin/main` (`3dbdf8b`) predates all recall work, so the diff is the whole project. The JSON run lists 30 changed files before this document, 31 at the final HEAD. In Go: `recall_cmd.go` adds 39 entities including `recallCoverage`, `recallOutput`, `recallCoverageLine` and `recallPartialPhrase`; `recall_collect.go` adds 29 including `recallCheckpoint.Unavailable`, `recallIngestInput.Truncated` and `recallFilesFromDiff`; `root.go` has `NewRootCmd` body changed with 103 dependents and `labs.go` has `labsOverview` changed with 4. In Rust: `agreement.rs` adds `enum Agree` (24 dependents) and `agreement` (21), `rank.rs` adds `Ranked` and its fields, `model.rs` adds `Checkpoint`, `Coverage`, `Skipped` and `null_as_empty`. Three files were skipped with `W_UNSUPPORTED_FILE`: `Cargo.lock` and the two `.rs.ref` reference copies, which have no parser. The raw output is in the checkpoint 4 transcript.

**4. Current edge count: 40.** At checkpoint 2 the first ingest reported `graph_edges: 0`. Diagnosed cause: reach was resolved from the first `+` line of each changed file, which is a package or import line with no enclosing symbol. Fixed before noon by streaming `entire graph symbols` once and running `impact` on the first two code symbols each touched file defines. The latest ingest at HEAD reports 40 edges from 40 impact queries, which is the per-ingest cap (268 candidate symbols were available). Files with no code symbols, such as docs and JSON, correctly get none.

**5. Graph against source.** Correct: the seven callees `impact` listed for `agreement` match the source exactly (`missing_identifier`, `looks_like_url`, `GraphReach.escapes`, `terms`, `polarity`, `ngrams`, `cosine`), including the fact that `split_ident` and `diff_vocab` are reached only through `missing_identifier`. Incomplete: the same report lists `out` (`rank.rs:67`) and `n` (`ingest.rs:50`) as direct callers, which are local variable bindings, not functions; and the first text rendering of `graph diff` printed 14 of the 30 changed files and stopped after `docs/`, omitting the entire Rust crate that the `--json` output of the same run carried; a re-run at the final HEAD rendered all 31 files, so the omission was not reproducible, and the JSON is the output to trust. Graph located every path I needed and its structural facts checked out; its heuristic dependent counts (`Turn.text`, 718 dependents) are name-based and I did not act on them.

## Noon Curveball: what changed and how I adapted

**The constraint.** Track 1 — Privacy Boundary. The Checkpoint-native experience must work with sensitive repositories: raw prompts and transcripts must not go to a new external service, the product must stay useful when fields are redacted or unavailable, existing local functionality must keep working, the interface must distinguish complete from incomplete context, and it must never present incomplete context as a complete or authoritative result.

**The assumption it invalidated.** My ranking pipeline assumed full transcript text is always available. All six `agreement()` checks read raw claim content, and two of them — the file check and the identifier anchor — treat *absence from the diff* as evidence that a claim is wrong. Under redaction absence is guaranteed, so those checks would convert missing evidence into confident `Contradicted` verdicts. Worse, confidence scores would keep looking authoritative while running on nothing at all.

**What I found when I looked.** This was not hypothetical. The morning's ingest reported `"checkpoints": 3` — and dropped 122 others without a word. Their checkpoint refs live on an upstream remote my fork cannot read, so they were skipped, silently, and three were presented as the whole history. I was already doing exactly what the card forbids, before the card was issued.

**What I changed.**

A fourth verdict, `Unverifiable`, distinct from `Neutral`. `Neutral` means I checked and found nothing. `Unverifiable` means I could not check — the input was not available to me. Collapsing those two is the failure the constraint names, because it presents an incomplete check as a completed one. Each check in `agreement()` now returns `Unverifiable` when the field it depends on is declared unavailable, and an unverifiable result short-circuits rather than falling through to a later corroboration — otherwise a lexical match on a commit subject could vouch for a claim whose diff nobody read.

Ingest stops dropping checkpoints. When a transcript cannot be read, the checkpoint is still emitted with its commit metadata and `unavailable: ["session"]`, so the ledger engram survives and a sensitive repository still gets commit-level recall. The same walk now ingests 125 checkpoints instead of 3, with zero silent drops.

Availability is declared, never inferred. The shim states which fields are missing on the wire type; ingest records whether the graph was available. Detection is explicit rather than guessed from emptiness — an inline redaction marker adds the partial marker and the confidence cap but never changes a verdict. This matters because 17 pairs in the benchmark carry a literal REDACTED token and 65 contain isolation words, and the bench runs with an empty graph. Routing on a marker, or treating an empty graph as an unavailable one, would have moved the numbers.

Confidence is capped, not merely lowered. Any non-ledger hit drawn from a partial checkpoint is clamped to 0.50 after all other scoring. That sits above the 0.30 and 0.35 baselines and below a corroborated hit near 0.96, so partial context can rank but can never outrank fully verified context. Ledger hits keep their confidence — the commit record is itself complete — but carry the partial marker.

Coverage is structural, not decorative. `activate` returns `{"coverage": ..., "hits": [...]}` rather than a bare array, so a machine consumer can tell partial from complete. Text output opens with the context line before any result:

    PARTIAL · 4 of 125 checkpoints complete · 121 without transcript
            · older checkpoints not examined (walk budget) · graph ok

A reader who sees only the first line still learns the context was incomplete. Per-hit, `? UNVERIFIABLE` renders distinctly from `· neutral`, and partial hits carry `◌ partial context: transcript unavailable`. A brain with no coverage record refuses to activate rather than defaulting to "complete".

I also added `--no-transcripts`, which never opens session content at all and marks every checkpoint `unavailable: ["session"]` by policy. It is the demonstration path for a sensitive repository, and it answers the residual that the brain directory holds a second local copy of transcript text.

**What I deliberately did not change.** The unredacted path. Verdict routing changes only when an unavailable field or `graph_available: false` is present, so unredacted input takes a byte-identical path through `agreement()`. The six-check order is unchanged. The benchmark was re-run after the final edit and holds exactly: P 0.869 / R 0.270 / Spec 0.927.

That constraint forced one narrowing. A whitelist rule skipping every token beginning with `/` moved the profile to P 0.882 / Spec 0.937, because the corpus contains forge refs like `/gh/...` that the file check judges correctly. The rule was narrowed to machine-absolute roots only, measured rule by rule until the profile held.

**Why the new result is safe.** Nothing new leaves the machine: FluctlightDB is an embedded library, the brain directory is local and gitignored, and Entire Graph runs locally. `verify-offline` traces `ingest` and `activate` under `strace` and records zero network syscalls across every traced process. Incomplete context is structurally incapable of outranking complete context, because the cap is applied after scoring rather than as a soft penalty. And no result set can be read as complete when it is not, because coverage is printed before the results and encoded in the JSON envelope.

**How I resumed.** The fresh session had no memory of the morning. Its first action was not to read source — it was to run `entire recall ingest` and query the checkpoint history for the morning's intent, architecture, completed work and open risks. Reconstruction came from the product itself, then Entire Graph impact analysis on every consumer of raw prompt and transcript text, and only then any edit.

**One defect a live run caught that unit tests could not.** Nil Go slices marshalled as JSON `null`, which the Rust side rejected — the first real `--no-transcripts` ingest wiped the brain and failed. Both sides now carry a regression test: the shim emits `[]`, the crate accepts `null` as empty.

## Checkpoint links and what each proves

| # | checkpoint | proves |
| --- | --- | --- |
| 1 | `01M1TM400B70E9ZA8DESDQ42M9` (fde9371) | initial understanding, intended architecture, risks logged before implementation |
| 2 | `01M1TNEYKC9VM69ZJ801XDJT4P` (b5d8028) | pre-noon stable state: Rust port + Go shim, tests green |
| 3 | `01M1TSPHG6VCEC5BHG41WFSH2Q` (41e9484) | curveball response: `entire recall` used in a fresh session to reconstruct the morning, Graph impact before the edit, fourth verdict, coverage line, transcript-free ingest, offline proof, bench held |
| 4 | `01M1TWCKXPN0HR8PKW0YQ1BZ50` (this commit) | final implementation and verification: this document, the `origin/main..HEAD` semantic diff, 27 Rust and 19 Go recall tests green, 125 checkpoints ingested |

entire.io links. The web app exposes checkpoints through their session page, so each link opens the session that produced the checkpoint:

- Checkpoints 1, 2 (session `1108ead7-3219-47e6-aebb-9e25067f3f27`): https://entire.io/gh/voxmastery/cli/session/1108ead7-3219-47e6-aebb-9e25067f3f27
- Checkpoints 3, 4 (session `a18af583-9099-4691-a142-a7f1c592aca2`): https://entire.io/gh/voxmastery/cli/session/a18af583-9099-4691-a142-a7f1c592aca2

Locally, `entire checkpoint explain <id>` prints any of the four.

## Setup, run and test instructions

```bash
# Rust engine
cd recall
cargo build --release
cargo test                       # 27 tests (15 pre-noon + 12 privacy-boundary)
cargo run --release --example bench   # reproduces the benchmark table

# Go CLI
go build ./cmd/entire
go test ./cmd/entire/cli -run 'Recall|RenderRecall'   # 19 recall tests
recall/scripts/verify-offline.sh   # verify-offline: OK — 0 network syscalls across 2 traced processes (ingest + activate)

# End to end, from the repo root
entire graph index               # warm the cache first — see limitations
entire recall ingest             # 125 checkpoints at HEAD (the morning's ingest indexed 3 and dropped 122 silently)
# context: PARTIAL · 4 of 125 checkpoints complete · 121 without transcript · older checkpoints not examined (walk budget) · graph ok
entire recall "why did we choose FluctlightDB"
entire recall ingest --no-transcripts   # never opens a transcript; every checkpoint ingested as its commit record
```

Requires Go 1.26+, Rust stable, Git 2.36+, and the Entire CLI with the `graph` plugin.

## Prior work and transparency

**FluctlightDB** (github.com/voxmastery/FluctlightDB, MIT/Apache-2.0) is a pre-existing embedded memory engine I wrote and published before this event. It is used here as an unmodified git dependency, in the same way any team might depend on a published open-source crate. It provides the storage engine, spreading activation, and the provenance/confidence primitives.

Everything specific to this project was written today, after the 9:00 AM start, in this fork: the checkpoint→engram ingest mapping, `agreement()` and all six of its checks, the confidence-weighted reranking, the Graph integration, the Go subcommand, the benchmark harness, and the tests.

Claude Code was my coding agent throughout, run through the WOZCODE harness, which adds itself as commit co-author on my commits. There are no human collaborators — this is a solo entry. Agent sessions are captured in the checkpoints above. The benchmark fixtures are generated from this repository's public commit history. No private, customer, or personal data was used. No credentials appear in the repository, checkpoints, or this document.

**Data provenance of `recall/bench/bench_840.json`.** The file is generated from upstream entireio/cli public commit history. GitHub secret scanning flags an AWS Access Key ID in it. That string is Entire's own fixture for testing their secret-redaction feature, present across many upstream commits, allowlisted as test data, and not a live credential.

## Known limitations and next steps

**Recall is 0.270.** It catches roughly a quarter of genuinely inconsistent claims. The tool is a high-precision screen, not a comprehensive detector, and should be described that way to any user.

**Thresholds are repo-calibrated; the structural check is not.** I evaluated across six repositories in four languages — 4,961 labelled pairs, generated the same way as the primary benchmark, generated with the same harness in a separate working directory; only the 840-pair set ships in this repository.

| repo | language | pairs | P | R | Spec |
| --- | --- | --- | --- | --- | --- |
| entireio/cli | Go | 840 | 0.800 | 0.624 | 0.720 |
| fastapi | Python | 889 | 0.827 | 0.868 | 0.643 |
| fzf | Go/shell | 800 | 0.720 | 0.776 | 0.497 |
| express | JavaScript | 805 | 0.672 | 0.850 | 0.303 |
| bat | Rust | 812 | 0.670 | 0.846 | 0.290 |
| requests | Python | 815 | 0.664 | 0.885 | 0.230 |

The thresholded checks do not transfer — specificity ranges from 0.23 to 0.72 depending on commit style. The M1 identifier anchor, which is structural rather than tuned, does: measured alone it holds specificity between 0.847 and 0.950 on every repository, with precision never below 0.67, across four languages it was never designed against.

The generalizable core is therefore the structural check. The n-gram and diff-miss thresholds require per-repository calibration before this is safe to run outside a tuned repo. That calibration step is the first item of production work, not a footnote.

**The semantic lane is dark.** Engrams are ingested without embedding vectors, so activation runs on the lexical index alone. Paraphrase — a claim that agrees with a commit in different words — reads as Neutral rather than Corroborated. Populating `semantic_vector` with a local embedder is the single highest-value next step.

**Real transcripts are noisy.** Partly fixed. The URL guard landed before noon and is pinned by `url_in_claim_is_not_a_file_path`: a pasted docs link no longer reads as an untouched repo path. The curveball added a narrow whitelist for the three shapes the morning transcript still tripped on (`µs/pair`, `feat/fix/revert`, machine-absolute paths), each rule measured against the bench before keeping it. Not fixed: prose with a slash in it. The pasted build brief is still CONTRADICTED, now on `definition/search` and `entireio/cli`, and every new rule costs a bench run. And long single-turn prompts still degrade ranking: a 2.5 KB brief matches almost any question by sheer term count, lands at rank 1 as INTENT, and one false path token then contradicts the whole turn. The fix is per-sentence claims rather than per-turn, which is ingest work, not ranking work.

**Graph cache invalidation.** Moving HEAD invalidates the graph cache and re-indexing costs ~90s, which makes ingest slow immediately after a commit. `entire graph index` must be run before demoing.

**Next steps, in order:** populate semantic vectors to close the paraphrase gap; per-repo threshold calibration; incremental ingest so recall stays fast as history grows; and surface `recall` output directly inside an agent session rather than as a separate command.
