//! File-level reach from `entire graph impact`.
//!
//! For each touched file we ask the graph what depends on the symbol at the
//! file's first changed line (callers, type consumers, data flows) and keep
//! the files those live in. That is the "blast radius" an isolation claim is
//! checked against. The graph is optional: when it is unavailable the reach
//! is empty and the scope check simply cannot fire.

use std::collections::{HashMap, HashSet};
use std::process::Command;

use serde_json::Value;

use crate::model::Checkpoint;

#[derive(Debug, Default, Clone)]
pub struct GraphReach {
    edges: HashMap<String, Vec<String>>,
}

impl GraphReach {
    pub fn from_map(edges: HashMap<String, Vec<String>>) -> Self {
        Self { edges }
    }

    pub fn reach(&self, file: &str) -> &[String] {
        self.edges.get(file).map(Vec::as_slice).unwrap_or(&[])
    }

    pub fn edge_count(&self) -> usize {
        self.edges.values().map(Vec::len).sum()
    }

    pub fn is_empty(&self) -> bool {
        self.edges.is_empty()
    }

    /// Files reached from `touched` that are not themselves touched.
    pub fn escapes(&self, touched: &[String]) -> Vec<String> {
        let set: HashSet<&str> = touched.iter().map(String::as_str).collect();
        let mut out: Vec<String> = touched
            .iter()
            .flat_map(|f| self.reach(f).iter())
            .filter(|d| !set.contains(d.as_str()))
            .cloned()
            .collect();
        out.sort();
        out.dedup();
        out
    }

    /// Query `entire graph impact` once per (file, first changed line) across
    /// all checkpoints. Failures are logged to stderr and skipped.
    pub fn from_entire_graph(repo_root: &str, cps: &[Checkpoint]) -> Self {
        let mut edges: HashMap<String, Vec<String>> = HashMap::new();
        let mut seen: HashSet<String> = HashSet::new();
        for cp in cps {
            let lines = first_changed_lines(&cp.diff);
            for f in &cp.files {
                if !seen.insert(f.clone()) {
                    continue;
                }
                let Some(line) = lines.get(f) else { continue };
                match impact(repo_root, f, *line) {
                    Ok(json) => {
                        let reached = parse_impact_json(f, &json);
                        if !reached.is_empty() {
                            edges.insert(f.clone(), reached);
                        }
                    }
                    Err(e) => eprintln!("recall: graph impact {f}:{line}: {e}"),
                }
            }
        }
        Self { edges }
    }
}

fn impact(repo_root: &str, file: &str, line: usize) -> Result<String, String> {
    let out = Command::new("entire")
        .args([
            "graph",
            "impact",
            "--repo",
            repo_root,
            "--head",
            "--format",
            "json",
            "--depth",
            "1",
            "--exclude-tests",
            "--limit",
            "30",
            "--symbol",
        ])
        .arg(format!("{file}:{line}"))
        .output()
        .map_err(|e| e.to_string())?;
    if !out.status.success() {
        return Err(String::from_utf8_lossy(&out.stderr).trim().to_string());
    }
    Ok(String::from_utf8_lossy(&out.stdout).into_owned())
}

/// Map each file in a unified diff to the first line number on its `+` side.
/// Files whose diff lacks hunk headers get no entry.
pub fn first_changed_lines(diff: &[String]) -> HashMap<String, usize> {
    let mut out = HashMap::new();
    let mut current: Option<String> = None;
    for line in diff {
        if let Some(rest) = line.strip_prefix("+++ ") {
            current = Some(rest.trim().trim_start_matches("b/").to_string());
        } else if let (Some(rest), Some(f)) = (line.strip_prefix("@@ "), &current) {
            if out.contains_key(f) {
                continue;
            }
            if let Some(n) = rest
                .split_whitespace()
                .find_map(|t| t.strip_prefix('+'))
                .and_then(|t| t.split(',').next())
                .and_then(|t| t.parse::<usize>().ok())
            {
                out.insert(f.clone(), n.max(1));
            }
        }
    }
    out
}

/// Extract the set of internal files an impact result reaches, excluding the
/// focus file itself and external (stdlib / dependency) endpoints.
pub fn parse_impact_json(file: &str, json: &str) -> Vec<String> {
    let Ok(v) = serde_json::from_str::<Value>(json) else {
        return Vec::new();
    };
    let mut files: HashSet<String> = HashSet::new();
    for section in ["callers", "type_consumers", "data_flows"] {
        let Some(entries) = v
            .get(section)
            .and_then(|s| s.get("entries"))
            .and_then(Value::as_array)
        else {
            continue;
        };
        for e in entries {
            let Some(ep) = e.get("endpoint") else {
                continue;
            };
            if ep.get("external").and_then(Value::as_bool).unwrap_or(false) {
                continue;
            }
            if let Some(p) = ep
                .get("file_path")
                .and_then(Value::as_str)
                .filter(|p| *p != file)
            {
                files.insert(p.to_string());
            }
        }
    }
    files.into_iter().collect()
}
