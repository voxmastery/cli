//! Rerank raw activations by provenance and ledger agreement.
//! Do NOT sort on raw activation.

use std::collections::{HashMap, HashSet};

use fluctlightdb::confidence::{Evidence, SourceKind, activation_multiplier, recall_confidence};
use fluctlightdb::types::{Provenance, ProvenanceKind, RecallResult};
use serde::{Deserialize, Serialize};

use crate::agreement::{Agree, agreement};
use crate::graph::GraphReach;
use crate::model::Checkpoint;

pub const CONTRADICTION_PENALTY: f32 = 0.45;

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Ranked {
    pub raw: f32,
    pub scored: f32,
    pub confidence: f32,
    pub tier: String,
    pub verdict: Agree,
    pub why: String,
    pub backed_by: Option<String>,
    pub commit: Option<String>,
    pub text: String,
    pub context: String,
}

fn source_kind(p: &Option<Provenance>) -> SourceKind {
    match p.as_ref().map(|x| x.kind.clone()) {
        Some(ProvenanceKind::LedgerVerified)
        | Some(ProvenanceKind::ToolGrounded)
        | Some(ProvenanceKind::FileObservation) => SourceKind::Verified,
        Some(ProvenanceKind::UserExplicit) => SourceKind::UserStated,
        _ => SourceKind::Unknown,
    }
}

pub fn commit_of(ctx: &str) -> Option<String> {
    ctx.split_whitespace()
        .find_map(|t| t.strip_prefix("commit:").map(|s| s.to_string()))
}

pub fn tier(kind: SourceKind) -> &'static str {
    match kind {
        SourceKind::Verified => "LEDGER",
        SourceKind::UserStated => "INTENT",
        SourceKind::Inferred => "infer",
        SourceKind::Unknown => "chat",
    }
}

pub fn rerank(recalls: &[RecallResult], cps: &[Checkpoint], g: &GraphReach) -> Vec<Ranked> {
    let by_sha: HashMap<&str, &Checkpoint> =
        cps.iter().map(|c| (c.commit_sha.as_str(), c)).collect();

    let mut out: Vec<Ranked> = recalls
        .iter()
        .map(|r| {
            let kind = source_kind(&r.episode.provenance);
            let mut ev = vec![Evidence::new(kind, 1.0)];
            let sha = commit_of(&r.episode.context);
            let cp = sha.as_deref().and_then(|k| by_sha.get(k).copied());
            let backed_by = cp.map(|c| format!("{} {}", c.commit_sha, c.commit_message));
            let (verdict, why) = match cp {
                Some(cp) if kind != SourceKind::Verified => agreement(&r.episode.content, cp, g),
                Some(_) => (Agree::Corroborated, "is the commit record".into()),
                None => (Agree::Neutral, "no commit context".into()),
            };
            match verdict {
                // Earned corroboration: an independent verified source.
                Agree::Corroborated if kind != SourceKind::Verified => {
                    ev.push(Evidence::new(SourceKind::Verified, 1.0))
                }
                // Refuted by the ledger: the claim is evidence against itself.
                Agree::Contradicted => ev.push(Evidence::new(SourceKind::Unknown, 0.15)),
                _ => {}
            }
            let mut confidence = recall_confidence(&ev);
            if verdict == Agree::Contradicted {
                confidence *= CONTRADICTION_PENALTY;
            }
            let scored = r.activation * activation_multiplier(confidence);
            Ranked {
                raw: r.activation,
                scored,
                confidence,
                tier: tier(kind).to_string(),
                verdict,
                why,
                backed_by,
                commit: sha,
                text: r.episode.content.replace('\n', " "),
                context: r.episode.context.clone(),
            }
        })
        .collect();
    out.sort_by(|a, b| {
        b.scored
            .partial_cmp(&a.scored)
            .unwrap_or(std::cmp::Ordering::Equal)
    });
    let mut seen = HashSet::new();
    out.retain(|r| seen.insert(r.text.clone()));
    out
}
