package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/object"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/stringutil"
	"github.com/entireio/cli/cmd/entire/cli/summarize"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
	"github.com/entireio/cli/redact"
)

// Wire shape consumed by recall/ (src/model.rs). Keep the two in step.
type recallTurn struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

type recallCheckpoint struct {
	CheckpointID  string       `json:"checkpoint_id"`
	CommitSHA     string       `json:"commit_sha"`
	CommitMessage string       `json:"commit_message"`
	Agent         string       `json:"agent"`
	Files         []string     `json:"files"`
	Session       []recallTurn `json:"session"`
	Diff          []string     `json:"diff"`
}

type recallIngestInput struct {
	RepoRoot    string             `json:"repo_root"`
	Checkpoints []recallCheckpoint `json:"checkpoints"`
}

// recallCommit is a commit on the branch that carries an Entire-Checkpoint trailer.
type recallCommit struct {
	SHA          string
	Message      string
	CheckpointID id.CheckpointID
}

const (
	recallMaxCommits        = 200
	recallMaxDiffLines      = 400
	recallMaxAssistantRunes = 600
)

// collectRecallCheckpoints walks HEAD's first-parent history for trailered
// commits and reads each one's checkpoint into the wire shape.
func collectRecallCheckpoints(ctx context.Context, errW io.Writer, root string) (*recallIngestInput, error) {
	lookup, err := newExplainCheckpointLookup(ctx)
	if err != nil {
		return nil, err
	}
	defer lookup.Close()

	head, err := lookup.repo.Head()
	if err != nil {
		return nil, fmt.Errorf("resolve HEAD: %w", err)
	}
	iter, err := lookup.repo.Log(&git.LogOptions{From: head.Hash(), Order: git.LogOrderCommitterTime})
	if err != nil {
		return nil, fmt.Errorf("walk history: %w", err)
	}
	defer iter.Close()

	input := &recallIngestInput{RepoRoot: root}
	seen := 0
	err = iter.ForEach(func(c *object.Commit) error {
		if seen >= recallMaxCommits {
			return errRecallWalkDone
		}
		seen++
		cpID, ok := trailers.ParseCheckpoint(c.Message)
		if !ok {
			return nil
		}
		diff, diffErr := recallCommitDiff(ctx, root, c.Hash.String())
		if diffErr != nil {
			fmt.Fprintf(errW, "recall: diff for %s: %v\n", c.Hash.String()[:7], diffErr)
		}
		cp, cpErr := recallCheckpointFromReader(ctx, lookup.store, recallCommit{SHA: c.Hash.String()[:7], Message: c.Message, CheckpointID: cpID}, diff)
		if cpErr != nil {
			fmt.Fprintf(errW, "recall: skipping %s: %v\n", cpID, cpErr)
			return nil
		}
		input.Checkpoints = append(input.Checkpoints, *cp)
		return nil
	})
	if err != nil && !errors.Is(err, errRecallWalkDone) {
		return nil, fmt.Errorf("walk history: %w", err)
	}
	return input, nil
}

var errRecallWalkDone = errors.New("recall: walk budget reached")

// recallReader is the slice of the persistent store the shim reads through.
type recallReader interface {
	Read(ctx context.Context, checkpointID id.CheckpointID) (*checkpoint.CheckpointSummary, error)
	ReadSessionContent(ctx context.Context, checkpointID id.CheckpointID, sessionIndex int) (*checkpoint.SessionContent, error)
}

// recallCheckpointFromReader reads the checkpoint summary and its latest
// session, and condenses the transcript into user/assistant turns.
func recallCheckpointFromReader(ctx context.Context, reader recallReader, commit recallCommit, diff []string) (*recallCheckpoint, error) {
	summary, err := reader.Read(ctx, commit.CheckpointID)
	if err != nil {
		return nil, fmt.Errorf("read checkpoint: %w", err)
	}
	if summary == nil || len(summary.Sessions) == 0 {
		return nil, checkpoint.ErrCheckpointNotFound
	}
	content, err := reader.ReadSessionContent(ctx, commit.CheckpointID, len(summary.Sessions)-1)
	if err != nil {
		return nil, fmt.Errorf("read session: %w", err)
	}
	files := summary.FilesTouched
	if len(files) == 0 {
		files = content.Metadata.FilesTouched
	}
	var turns []recallTurn
	if entries, condErr := summarize.BuildCondensedTranscriptFromBytes(redact.AlreadyRedacted(content.Transcript), content.Metadata.Agent); condErr == nil {
		turns = recallTurnsFromEntries(entries, recallMaxAssistantRunes)
	}
	subject, _, _ := strings.Cut(strings.TrimSpace(commit.Message), "\n")
	return &recallCheckpoint{
		CheckpointID:  commit.CheckpointID.String(),
		CommitSHA:     commit.SHA,
		CommitMessage: subject,
		Agent:         string(content.Metadata.Agent),
		Files:         files,
		Session:       turns,
		Diff:          diff,
	}, nil
}

// recallTurnsFromEntries keeps user and assistant text, collapses whitespace,
// and truncates assistant turns (tool-heavy and long) but never user intent.
func recallTurnsFromEntries(entries []summarize.Entry, maxAssistantRunes int) []recallTurn {
	turns := make([]recallTurn, 0, len(entries))
	for _, e := range entries {
		text := stringutil.CollapseWhitespace(e.Content)
		if text == "" {
			continue
		}
		switch e.Type {
		case summarize.EntryTypeUser:
			turns = append(turns, recallTurn{Role: "user", Text: text})
		case summarize.EntryTypeAssistant:
			turns = append(turns, recallTurn{Role: "assistant", Text: stringutil.TruncateRunes(text, maxAssistantRunes, "...")})
		case summarize.EntryTypeTool:
		}
	}
	return turns
}

// recallCommitDiff returns the commit's unified-0 diff as filtered lines.
func recallCommitDiff(ctx context.Context, root, sha string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", root, "show", "--format=", "--no-color", "-U0", sha)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git show: %w", err)
	}
	return recallDiffLines(string(out), recallMaxDiffLines), nil
}

// recallDiffLines keeps the lines the Rust side reads — `+++` file headers,
// `@@` hunks, and +/- content — and drops index/`---`/no-newline noise.
func recallDiffLines(raw string, maxLines int) []string {
	var out []string
	for line := range strings.SplitSeq(raw, "\n") {
		if len(out) >= maxLines {
			break
		}
		switch {
		case strings.HasPrefix(line, "+++ "), strings.HasPrefix(line, "@@ "):
			out = append(out, line)
		case strings.HasPrefix(line, "--- "), strings.HasPrefix(line, "\\ "), line == "":
		case line[0] == '+' || line[0] == '-':
			out = append(out, line)
		}
	}
	return out
}
