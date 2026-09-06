//! Semantic agreement: does a claim match what the commit it sits on did?
//! Corroboration is earned, not granted for merely sitting on a commit.

use crate::graph::GraphReach;
use crate::model::Checkpoint;
use crate::text::{code_shaped, cosine, diff_vocab, ngrams, polarity, split_ident, terms};

/// Three-way verdict of a claim against the commit it sits on.
#[derive(PartialEq, Eq, Clone, Copy, Debug, serde::Serialize, serde::Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum Agree {
    Corroborated,
    Neutral,
    Contradicted,
}

const ISOLATION: &[&str] = &[
    "isolated",
    "does not touch",
    "nothing else",
    "contained",
    "no other",
];
const NGRAM_THRESHOLD: f32 = 0.10;
const LEXICAL_MIN_SHARED: usize = 2;

/// Checks run in this order; the first that fires decides.
pub fn agreement(claim: &str, cp: &Checkpoint, g: &GraphReach) -> (Agree, String) {
    let low = claim.to_lowercase();

    // 1. Scope: claim asserts isolation, Graph says the blast radius escapes.
    if ISOLATION.iter().any(|w| low.contains(w)) {
        let reach = g.escapes(&cp.files);
        if !reach.is_empty() {
            return (
                Agree::Contradicted,
                format!(
                    "claims isolation; Graph reach escapes to {} ({})",
                    reach.len(),
                    reach.join(", ")
                ),
            );
        }
    }

    // 2. File: claim names a path the commit never touched. URL segments are
    //    not paths: a pasted link must not read as an untouched file.
    let claim_terms = terms(claim);
    for t in claim_terms
        .iter()
        .filter(|t| (t.contains('/') || t.ends_with(".go")) && !looks_like_url(t))
    {
        if !cp
            .files
            .iter()
            .any(|f| f.to_lowercase().contains(t.as_str()))
        {
            return (
                Agree::Contradicted,
                format!("references {t} which this commit did not touch"),
            );
        }
    }

    // 3. Polarity flip: same topic, opposite action. Conventional-commit
    //    prefixes carry polarity the verb list misses.
    let pc = polarity(claim);
    let mut pm = polarity(&cp.commit_message);
    if pm == 0 {
        let m = cp.commit_message.to_lowercase();
        if m.starts_with("feat") || m.starts_with("fix") || m.starts_with("perf") {
            pm = 1;
        } else if m.starts_with("revert") {
            pm = -1;
        }
    }
    let commit_terms = terms(&cp.commit_message);
    if pc != 0 && pm != 0 && pc != pm && claim_terms.intersection(&commit_terms).count() >= 1 {
        return (
            Agree::Contradicted,
            "action polarity opposes the commit".into(),
        );
    }

    // 4. M1 identifier anchor: a code-shaped token the diff never contains.
    if let Some(ident) = missing_identifier(claim, cp) {
        return (
            Agree::Contradicted,
            format!("names {ident}, which the change does not contain"),
        );
    }

    // 5. Lexical agreement with what the commit says it did.
    let shared = claim_terms.intersection(&commit_terms).count();
    if shared >= LEXICAL_MIN_SHARED {
        return (
            Agree::Corroborated,
            format!("{shared} terms match commit subject"),
        );
    }

    // 6. Character 4-gram cosine against the diff and file names.
    let corpus = format!(
        "{} {} {}",
        cp.commit_message,
        cp.files.join(" "),
        cp.diff.join(" ")
    );
    let sim = cosine(&ngrams(claim), &ngrams(&corpus));
    if sim > NGRAM_THRESHOLD {
        return (
            Agree::Corroborated,
            format!("4-gram cosine {sim:.3} with the change"),
        );
    }

    (Agree::Neutral, "no overlap with commit subject".into())
}

/// M1 — identifier anchoring. Words that look like code are hard claims: if
/// the message names one and the diff never contains any of its pieces, the
/// message is lying about the code. Returns the first offending token.
fn missing_identifier(claim: &str, cp: &Checkpoint) -> Option<String> {
    let v = diff_vocab(cp);
    for raw in claim.split(|ch: char| ch.is_whitespace() || ch == '`' || ch == ',' || ch == ':') {
        let w = raw.trim_matches(|ch: char| {
            !ch.is_alphanumeric() && ch != '_' && ch != '.' && ch != '(' && ch != ')'
        });
        // A URL is dotted and slashed, so it looks like code; it is not a claim
        // about the change (same guard as the file check).
        if w.len() < 5 || !code_shaped(w) || looks_like_url(w) {
            continue;
        }
        let parts = split_ident(w);
        if !parts.is_empty() && parts.iter().all(|p| !v.contains(p)) {
            return Some(w.to_string());
        }
    }
    None
}

const TLDS: &[&str] = &[".com", ".io", ".dev", ".org", ".net", ".ai", ".co"];

/// A term is URL-shaped when it carries a scheme, a `www.` host, or a
/// TLD-looking suffix on its first segment. `terms()` splits on `:` so a URL
/// arrives as `https` plus `//host/path...`; the leading `//` marks the latter.
fn looks_like_url(t: &str) -> bool {
    if t.contains("://") || t.starts_with("//") || t.starts_with("www.") {
        return true;
    }
    let host = t.split('/').next().unwrap_or(t);
    TLDS.iter()
        .any(|tld| host.ends_with(tld) || host.contains(&format!("{tld}/")))
        || TLDS.iter().any(|tld| t.contains(&format!("{tld}/")))
}
