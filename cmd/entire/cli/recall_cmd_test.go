package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/experimental"
	"github.com/entireio/cli/cmd/entire/cli/summarize"
	"github.com/stretchr/testify/require"
)

func TestRecallCommandIsExperimentalAndListedInLabs(t *testing.T) {
	t.Parallel()

	root := NewRootCmd()
	cmd, _, err := root.Find([]string{"recall"})
	require.NoError(t, err)
	require.Equal(t, "recall", cmd.Name())
	require.Equal(t, experimental.GroupID, cmd.GroupID, "recall must be gated as experimental")
	require.Contains(t, labsOverview(), "entire recall")

	ingest, _, err := root.Find([]string{"recall", "ingest"})
	require.NoError(t, err)
	require.Equal(t, "ingest", ingest.Name())
}

func TestRecallTurnsFromEntries_KeepsOnlyUserAndAssistantText(t *testing.T) {
	t.Parallel()

	entries := []summarize.Entry{
		{Type: summarize.EntryTypeUser, Content: "  Add a retry\n  wrapper  "},
		{Type: summarize.EntryTypeTool, ToolName: "Read", ToolDetail: "x.go"},
		{Type: summarize.EntryTypeAssistant, Content: strings.Repeat("a", 50)},
		{Type: summarize.EntryTypeAssistant, Content: "   "},
	}
	turns := recallTurnsFromEntries(entries, 20)

	require.Equal(t, []recallTurn{
		{Role: "user", Text: "Add a retry wrapper"},
		{Role: "assistant", Text: strings.Repeat("a", 17) + "..."},
	}, turns, "tool entries and blank turns are dropped; assistant text is truncated, user text is not")
}

func TestRecallDiffLines_KeepsHunksAndChangesDropsNoise(t *testing.T) {
	t.Parallel()

	raw := strings.Join([]string{
		"diff --git a/internal/x.go b/internal/x.go",
		"index 0000000..1111111 100644",
		"--- a/internal/x.go",
		"+++ b/internal/x.go",
		"@@ -0,0 +1,2 @@",
		"+func withRetry() {}",
		"-old := 1",
		"\\ No newline at end of file",
	}, "\n")
	got := recallDiffLines(raw, 100)

	require.Equal(t, []string{
		"+++ b/internal/x.go",
		"@@ -0,0 +1,2 @@",
		"+func withRetry() {}",
		"-old := 1",
	}, got)
}

func TestRecallDiffLines_IsCapped(t *testing.T) {
	t.Parallel()

	var sb strings.Builder
	for range 50 {
		sb.WriteString("+line\n")
	}
	require.Len(t, recallDiffLines(sb.String(), 10), 10)
}

func TestRecallBinary_PrefersEnvThenPathThenCargoTarget(t *testing.T) {
	t.Parallel()

	exists := func(paths ...string) func(string) bool {
		return func(p string) bool {
			for _, want := range paths {
				if p == want {
					return true
				}
			}
			return false
		}
	}
	noPath := func(string) (string, error) { return "", errors.New("not found") }

	got, err := recallBinary("/repo", "/opt/recall", noPath, exists("/opt/recall"))
	require.NoError(t, err)
	require.Equal(t, "/opt/recall", got, "ENTIRE_RECALL_BIN wins")

	got, err = recallBinary("/repo", "", func(string) (string, error) { return "/usr/bin/recall", nil }, exists())
	require.NoError(t, err)
	require.Equal(t, "/usr/bin/recall", got, "then $PATH")

	got, err = recallBinary("/repo", "", noPath, exists("/repo/recall/target/debug/recall"))
	require.NoError(t, err)
	require.Equal(t, "/repo/recall/target/debug/recall", got, "then the cargo build in the repo")

	_, err = recallBinary("/repo", "", noPath, exists())
	require.Error(t, err)
	require.Contains(t, err.Error(), "cargo build", "the error tells the user how to get a binary")
}

func TestRecallCheckpointFromReader_MapsSummaryAndLatestSessionToWireShape(t *testing.T) {
	t.Parallel()

	cpID := id.MustCheckpointID("abcd12345678")
	transcript := []byte(`{"type":"user","message":{"role":"user","content":"Add a retry wrapper around the client"},"uuid":"u1"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Adding retry.go now."}]},"uuid":"a1"}
`)
	reader := &stubCommittedReader{
		summary: &checkpoint.CheckpointSummary{
			CheckpointID: cpID,
			FilesTouched: []string{"internal/dispatch/retry.go"},
			Sessions:     []checkpoint.SessionFilePaths{{Metadata: "ab/cd12345678/0/metadata.json"}},
		},
		contents: map[int]*checkpoint.SessionContent{
			0: {
				Metadata:   checkpoint.Metadata{SessionID: "s1", Agent: "Claude Code"},
				Transcript: transcript,
			},
		},
	}
	commit := recallCommit{SHA: "9f2c1ab", Message: "feat: add retry wrapper\n\nEntire-Checkpoint: abcd12345678\n", CheckpointID: cpID}

	got, err := recallCheckpointFromReader(context.Background(), reader, commit, []string{"+func withRetry() {}"})
	require.NoError(t, err)

	require.Equal(t, "abcd12345678", got.CheckpointID)
	require.Equal(t, "9f2c1ab", got.CommitSHA)
	require.Equal(t, "feat: add retry wrapper", got.CommitMessage, "subject only: the trailer must not leak into the ledger text")
	require.Equal(t, "Claude Code", got.Agent)
	require.Equal(t, []string{"internal/dispatch/retry.go"}, got.Files)
	require.Equal(t, []string{"+func withRetry() {}"}, got.Diff)
	require.Equal(t, []recallTurn{
		{Role: "user", Text: "Add a retry wrapper around the client"},
		{Role: "assistant", Text: "Adding retry.go now."},
	}, got.Session)
}

func TestRenderRecallHits_ShowsTierVerdictAndBackingCommit(t *testing.T) {
	t.Parallel()

	backed := "77b4102 chore: drop retry wrapper"
	hits := []recallHit{
		{Tier: "INTENT", Verdict: "corroborated", Scored: 9.73, Confidence: 0.98, Text: "Remove the retry wrapper.", Why: "3 terms match commit subject", BackedBy: &backed},
		{Tier: "chat", Verdict: "contradicted", Scored: 1.20, Confidence: 0.21, Text: "This is isolated.", Why: "claims isolation; Graph reach escapes to 2"},
	}
	var out bytes.Buffer
	renderRecallHits(&out, "why did we drop the retry wrapper", hits)
	s := out.String()

	for _, want := range []string{
		"why did we drop the retry wrapper",
		"INTENT", "✓ corroborated", "Remove the retry wrapper.", "commit 77b4102 chore: drop retry wrapper",
		"chat", "✗ CONTRADICTED", "Graph reach escapes",
	} {
		require.Contains(t, s, want)
	}
	require.Less(t, strings.Index(s, "INTENT"), strings.Index(s, "chat"), "hits print in ranked order")
}

func TestRenderRecallHits_EmptySaysSo(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	renderRecallHits(&out, "anything", nil)
	require.Contains(t, out.String(), "no memories")
}
