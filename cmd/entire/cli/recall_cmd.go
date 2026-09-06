package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// recallBrainDir is the derived-state directory under .entire (gitignored).
const recallBrainDir = ".entire/recall/brain"

// recallHit is one ranked memory as emitted by the Rust binary. The shim
// renders it and never re-scores it: every ranking decision lives in recall/.
type recallHit struct {
	Raw        float64 `json:"raw"`
	Scored     float64 `json:"scored"`
	Confidence float64 `json:"confidence"`
	Tier       string  `json:"tier"`
	Verdict    string  `json:"verdict"`
	Why        string  `json:"why"`
	BackedBy   *string `json:"backed_by"`
	Commit     *string `json:"commit"`
	Text       string  `json:"text"`
	Context    string  `json:"context"`
}

func newRecallCmd() *cobra.Command {
	var k int
	var jsonFlag bool

	cmd := &cobra.Command{
		Use:         "recall <question>",
		Annotations: map[string]string{agentHelpAnnotation: agentHelpAnnotationEnabled},
		Hidden:      true,
		Short:       "Ask checkpoint history what was decided and why",
		Long: `Recall turns this branch's checkpoint history into associative memory and
answers a question with ranked, trust-scored hits: each carries a tier
(LEDGER = commit record, INTENT = user prompt, chat = assistant claim), a
verdict of the claim against the commit it sits on (corroborated,
unverified, or CONTRADICTED), and the backing commit.

Run 'entire recall ingest' first to build the memory under .entire/recall.`,
		Example: "  entire recall ingest\n  entire recall why did we drop the retry wrapper\n  entire recall --json what is unfinished in dispatch",
		Args:    cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRecallActivate(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), strings.Join(args, " "), k, jsonFlag)
		},
	}
	cmd.Flags().IntVar(&k, "k", 8, "Number of memories to return")
	cmd.Flags().BoolVar(&jsonFlag, "json", false, "Emit the ranked hits as JSON")

	var noGraph bool
	ingest := &cobra.Command{
		Use:   "ingest",
		Short: "Build recall memory from this branch's checkpoints",
		Long: `Walks the commits on the current branch that carry an Entire-Checkpoint
trailer, reads each checkpoint's transcript and diff, asks the code graph for
each changed file's blast radius, and writes the memory to .entire/recall.
The directory is derived state and is rebuilt from scratch on every run.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runRecallIngest(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), noGraph)
		},
	}
	ingest.Flags().BoolVar(&noGraph, "no-graph", false, "Skip 'entire graph impact' (faster; disables the isolation check)")
	cmd.AddCommand(ingest)
	return cmd
}

func runRecallIngest(ctx context.Context, w, errW io.Writer, noGraph bool) error {
	root, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return fmt.Errorf("not a git repository: %w", err)
	}
	input, err := collectRecallCheckpoints(ctx, errW, root)
	if err != nil {
		return err
	}
	if len(input.Checkpoints) == 0 {
		fmt.Fprintln(w, "No commits with an Entire-Checkpoint trailer on this branch; nothing to ingest.")
		return nil
	}
	payload, err := json.Marshal(input)
	if err != nil {
		return fmt.Errorf("encode ingest input: %w", err)
	}
	args := []string{"ingest", "--brain", filepath.Join(root, recallBrainDir)}
	if noGraph {
		args = append(args, "--no-graph")
	}
	out, err := runRecallBinary(ctx, root, errW, bytes.NewReader(payload), args...)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "Ingested %d checkpoints into %s\n%s", len(input.Checkpoints), recallBrainDir, out)
	return nil
}

func runRecallActivate(ctx context.Context, w, errW io.Writer, question string, k int, asJSON bool) error {
	root, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return fmt.Errorf("not a git repository: %w", err)
	}
	brain := filepath.Join(root, recallBrainDir)
	if _, statErr := os.Stat(brain); statErr != nil {
		return errors.New("no recall memory yet; run 'entire recall ingest' first")
	}
	out, err := runRecallBinary(ctx, root, errW, nil, "activate", "--brain", brain, "--k", strconv.Itoa(k), question)
	if err != nil {
		return err
	}
	if asJSON {
		_, err = io.WriteString(w, out)
		return err //nolint:wrapcheck // raw passthrough of the binary's JSON
	}
	var hits []recallHit
	if err := json.Unmarshal([]byte(out), &hits); err != nil {
		return fmt.Errorf("parse recall output: %w", err)
	}
	renderRecallHits(w, question, hits)
	return nil
}

// runRecallBinary executes the Rust binary with stdin and returns its stdout.
// Its stderr is forwarded so graph warnings reach the user.
func runRecallBinary(ctx context.Context, root string, errW io.Writer, stdin io.Reader, args ...string) (string, error) {
	bin, err := recallBinary(root, os.Getenv("ENTIRE_RECALL_BIN"), exec.LookPath, func(p string) bool {
		_, statErr := os.Stat(p) //nolint:gosec // p is $ENTIRE_RECALL_BIN (the user's own choice) or a fixed name under the worktree root
		return statErr == nil
	})
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = root
	cmd.Stdin = stdin
	cmd.Stderr = errW
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("recall %s failed: %w", args[0], err)
	}
	return string(out), nil
}

// recallBinary locates the Rust binary: $ENTIRE_RECALL_BIN, then `recall` on
// $PATH, then the cargo build inside the repository.
func recallBinary(root, envBin string, lookPath func(string) (string, error), exists func(string) bool) (string, error) {
	if envBin != "" {
		if exists(envBin) {
			return envBin, nil
		}
		return "", fmt.Errorf("ENTIRE_RECALL_BIN=%s does not exist", envBin)
	}
	if p, err := lookPath("recall"); err == nil {
		return p, nil
	}
	for _, profile := range []string{"release", "debug"} {
		p := filepath.Join(root, "recall", "target", profile, "recall")
		if exists(p) {
			return p, nil
		}
	}
	return "", errors.New("recall binary not found: run `cargo build --release` in recall/ or set ENTIRE_RECALL_BIN")
}

func renderRecallHits(w io.Writer, question string, hits []recallHit) {
	fmt.Fprintf(w, "recall: %q\n\n", question)
	if len(hits) == 0 {
		fmt.Fprintln(w, "  no memories matched; try different words or run 'entire recall ingest' again")
		return
	}
	for i, h := range hits {
		mark := map[string]string{
			"corroborated": "✓ corroborated",
			"contradicted": "✗ CONTRADICTED",
		}[h.Verdict]
		if mark == "" {
			mark = "· unverified"
		}
		fmt.Fprintf(w, "%2d. [%-6s] score %.2f  conf %.2f  %s\n", i+1, h.Tier, h.Scored, h.Confidence, h.Text)
		fmt.Fprintf(w, "    %s — %s\n", mark, h.Why)
		if h.BackedBy != nil {
			fmt.Fprintf(w, "    commit %s\n", *h.BackedBy)
		}
		fmt.Fprintln(w)
	}
}
