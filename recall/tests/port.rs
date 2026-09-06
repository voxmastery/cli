//! The seven reference tests, ported verbatim against the library API, plus
//! the checks the buildathon brief adds (M1 identifier anchor, graph-impact
//! parsing, check ordering).

use fluctlightdb::brain::FluctlightBrain;
use fluctlightdb::confidence::{Evidence, SourceKind, recall_confidence};
use recall::agreement::{Agree, agreement};
use recall::graph::{GraphReach, parse_impact_json};
use recall::ingest::ingest;
use recall::model::Checkpoint;
use recall::text::polarity;
use std::collections::HashMap;

fn cps() -> Vec<Checkpoint> {
    serde_json::from_str(&std::fs::read_to_string("fixtures/checkpoints.json").unwrap()).unwrap()
}
fn cp(sha: &str) -> Checkpoint {
    cps().into_iter().find(|c| c.commit_sha == sha).unwrap()
}

/// The fixture's reach map, identical to the reference stub: retry.go and
/// client.go escape the dispatch package.
fn fixture_graph() -> GraphReach {
    GraphReach::from_map(HashMap::from([
        (
            "internal/dispatch/retry.go".to_string(),
            vec![
                "internal/hooks/runner.go".to_string(),
                "internal/telemetry/emit.go".to_string(),
            ],
        ),
        (
            "internal/dispatch/client.go".to_string(),
            vec![
                "internal/dispatch/generate.go".to_string(),
                "internal/hooks/runner.go".to_string(),
            ],
        ),
    ]))
}

#[test]
fn wrapper_does_not_read_as_the_verb_wrap() {
    // regression: substring matching cancelled polarity to zero
    assert_eq!(polarity("Removing the retry wrapper entirely"), -1);
    assert_eq!(polarity("Add a retry wrapper"), 1);
}

#[test]
fn isolation_claim_refuted_by_graph_reach() {
    let (v, _) = agreement(
        "This is contained within internal/dispatch and does not touch callers.",
        &cp("9f2c1ab"),
        &fixture_graph(),
    );
    assert_eq!(v, Agree::Contradicted);
}

#[test]
fn polarity_flip_against_conventional_commit() {
    let (v, _) = agreement(
        "Dropped the empty-repo guard from dispatch generation.",
        &cp("5c81f33"),
        &fixture_graph(),
    );
    assert_eq!(v, Agree::Contradicted);
}

#[test]
fn untouched_file_is_contradiction() {
    let (v, _) = agreement(
        "Edited internal/auth/token.go to fix the guard.",
        &cp("5c81f33"),
        &fixture_graph(),
    );
    assert_eq!(v, Agree::Contradicted);
}

#[test]
fn agreeing_claim_is_corroborated() {
    let (v, _) = agreement(
        "Guard dispatch generation against empty repositories.",
        &cp("5c81f33"),
        &fixture_graph(),
    );
    assert_eq!(v, Agree::Corroborated);
}

#[test]
fn contradiction_collapses_confidence_below_corroborated() {
    let ok = recall_confidence(&[
        Evidence::new(SourceKind::UserStated, 1.0),
        Evidence::new(SourceKind::Verified, 1.0),
    ]);
    let bad = recall_confidence(&[
        Evidence::new(SourceKind::UserStated, 1.0),
        Evidence::new(SourceKind::Unknown, 0.15),
    ]) * 0.45;
    assert!(
        bad < ok,
        "contradicted {bad} must rank under corroborated {ok}"
    );
}

#[test]
fn brain_survives_reopen() {
    let dir = std::env::temp_dir().join("recall-test-brain");
    let _ = std::fs::remove_dir_all(&dir);
    let mut b = FluctlightBrain::open(&dir).unwrap();
    ingest(&mut b, &cps(), &fixture_graph());
    b.checkpoint().unwrap();
    drop(b);
    let r = FluctlightBrain::open_readonly(&dir).unwrap();
    assert!(!r.activate("retry wrapper").recalls.is_empty());
}

// ── additions from the brief ─────────────────────────────────────────────

#[test]
fn identifier_absent_from_diff_is_contradiction() {
    // M1: a code-shaped token the diff never introduces refutes the claim.
    let (v, why) = agreement(
        "Added the guard in generate.go via checkEmptyRepo() and setMaxRetries.",
        &cp("5c81f33"),
        &fixture_graph(),
    );
    assert_eq!(v, Agree::Contradicted, "{why}");
}

#[test]
fn identifier_present_in_diff_is_not_refuted_by_m1() {
    // withRetry and maxAttempts both appear on + lines of 9f2c1ab.
    let (v, why) = agreement(
        "Implemented withRetry with maxAttempts of three.",
        &cp("9f2c1ab"),
        &fixture_graph(),
    );
    assert_ne!(v, Agree::Contradicted, "{why}");
}

#[test]
fn paraphrase_is_corroborated_by_ngram_overlap() {
    // No 2-term lexical overlap with the subject, but the diff vocabulary
    // (backoff, attempt, retries) carries it over the 4-gram threshold.
    let (v, why) = agreement(
        "Introduced automatic re-attempts when the send fails, backing off each time.",
        &cp("9f2c1ab"),
        &fixture_graph(),
    );
    assert_eq!(v, Agree::Corroborated, "{why}");
}

#[test]
fn unrelated_claim_stays_neutral() {
    let (v, why) = agreement(
        "Ran the formatter across the package.",
        &cp("9f2c1ab"),
        &fixture_graph(),
    );
    assert_eq!(v, Agree::Neutral, "{why}");
}

#[test]
fn impact_json_yields_file_edges_excluding_self_and_externals() {
    let json = r#"{"focus":{"file_path":"internal/dispatch/retry.go"},
      "callers":{"entries":[
        {"endpoint":{"file_path":"internal/hooks/runner.go","external":false}},
        {"endpoint":{"file_path":"internal/dispatch/retry.go"}},
        {"endpoint":{"qualified_name":"fmt.Errorf","external":true}}]},
      "type_consumers":{"entries":[{"endpoint":{"file_path":"internal/telemetry/emit.go"}}]}}"#;
    let mut reached = parse_impact_json("internal/dispatch/retry.go", json);
    reached.sort();
    assert_eq!(
        reached,
        vec!["internal/hooks/runner.go", "internal/telemetry/emit.go"]
    );
}
