//! Wire types shared with the Go shim.

use serde::{Deserialize, Serialize};

#[derive(Debug, Deserialize, Serialize, Clone)]
pub struct Turn {
    pub role: String,
    pub text: String,
}

#[derive(Debug, Deserialize, Serialize, Clone)]
pub struct Checkpoint {
    pub checkpoint_id: String,
    pub commit_sha: String,
    pub commit_message: String,
    pub agent: String,
    pub files: Vec<String>,
    pub session: Vec<Turn>,
    /// Unified diff lines (`+`/`-` prefixed; hunk headers tolerated).
    #[serde(default)]
    pub diff: Vec<String>,
}

/// Everything `recall ingest` needs on stdin.
#[derive(Debug, Deserialize, Serialize, Clone)]
pub struct IngestInput {
    /// Repository root, used to run `entire graph impact`. Empty disables the graph.
    #[serde(default)]
    pub repo_root: String,
    pub checkpoints: Vec<Checkpoint>,
}

#[derive(Debug, Serialize, Deserialize, Clone)]
pub struct IngestReport {
    pub checkpoints: usize,
    pub engrams: usize,
    pub graph_edges: usize,
    pub brain: String,
}
