package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/tae2089/thread-keep/internal/app"
	"github.com/tae2089/thread-keep/internal/domain"
)

const OutputVersion = 1

type ServiceOpener func(context.Context, string) (*app.Service, error)

type Runner struct {
	open ServiceOpener
}

type operation func(context.Context, *app.Service, *cobra.Command, []string) (any, error)

func NewRunner(open ServiceOpener) *Runner {
	return &Runner{open: open}
}

func (r *Runner) Execute(ctx context.Context, root *cobra.Command, arguments []string, stdout, stderr io.Writer) int {
	if ctx == nil {
		ctx = context.Background()
	}
	root.SetArgs(arguments)
	root.SetOut(stdout)
	root.SetErr(stderr)
	if err := root.ExecuteContext(ctx); err != nil {
		if domain.CodeOf(err) == "" {
			err = domain.NewError(domain.CodeValidation, err)
		}
		jsonOutput, flagErr := root.PersistentFlags().GetBool("json")
		if flagErr != nil {
			jsonOutput = false
		}
		r.writeError(stderr, jsonOutput, err)
		return exitCode(err)
	}
	return 0
}

func (r *Runner) withService(run operation) func(*cobra.Command, []string) error {
	return func(command *cobra.Command, arguments []string) error {
		if r.open == nil {
			return domain.NewError(domain.CodeLocalStorage, fmt.Errorf("CLI service opener is not configured"))
		}
		workingDirectory, err := command.Root().PersistentFlags().GetString("repo")
		if err != nil {
			return err
		}
		if workingDirectory == "" {
			workingDirectory, err = os.Getwd()
			if err != nil {
				return err
			}
		}
		service, err := r.open(command.Context(), workingDirectory)
		if err != nil {
			return err
		}
		defer service.Close()
		result, err := run(command.Context(), service, command, arguments)
		if err != nil {
			return err
		}
		jsonOutput, err := command.Root().PersistentFlags().GetBool("json")
		if err != nil {
			return err
		}
		return writeResult(command.OutOrStdout(), jsonOutput, result)
	}
}

func writeResult(writer io.Writer, jsonOutput bool, result any) error {
	if jsonOutput {
		return json.NewEncoder(writer).Encode(struct {
			Version int `json:"version"`
			Data    any `json:"data"`
		}{Version: OutputVersion, Data: result})
	}
	switch value := result.(type) {
	case domain.Status:
		if _, err := fmt.Fprintf(writer, "ref: %s\nsource: %s\nentities: %d\npending notes: %d\ncontext commit: %s\ncoverage complete: %t\n", value.RefName, value.SourceSHA, value.EntityCount, value.PendingNotes, value.ContextCommitID, value.CoverageComplete); err != nil {
			return err
		}
		for _, coverage := range value.Coverage {
			if _, err := fmt.Fprintf(writer, "coverage: %s\t%s\t%s\n", coverage.Language, coverage.State, coverage.IndexerID); err != nil {
				return err
			}
		}
		return nil
	case app.UpdateResult:
		if _, err := fmt.Fprintf(writer, "indexed %d entities at %s\ncoverage complete: %t\n", value.IndexedEntities, value.SourceSHA, value.CoverageComplete); err != nil {
			return err
		}
		for _, coverage := range value.Coverage {
			if _, err := fmt.Fprintf(writer, "coverage: %s\t%s\t%s\n", coverage.Language, coverage.State, coverage.IndexerID); err != nil {
				return err
			}
		}
		return nil
	case app.RebuildResult:
		if _, err := fmt.Fprintf(writer, "restored %d context commits at %s\nindexed %d entities at %s\ncoverage complete: %t\n", value.RestoredCommits, value.ContextCommitID, value.IndexedEntities, value.SourceSHA, value.CoverageComplete); err != nil {
			return err
		}
		for _, coverage := range value.Coverage {
			if _, err := fmt.Fprintf(writer, "coverage: %s\t%s\t%s\n", coverage.Language, coverage.State, coverage.IndexerID); err != nil {
				return err
			}
		}
		return nil
	case []domain.IndexerStatus:
		for _, status := range value {
			if status.Path == "" {
				if _, err := fmt.Fprintf(writer, "%s\t%s\t%s\tdetected:%t\n", status.Language, status.State, status.PackID, status.Detected); err != nil {
					return err
				}
				continue
			}
			if _, err := fmt.Fprintf(writer, "%s\t%s\t%s\tdetected:%t\t%s\n", status.Language, status.State, status.PackID, status.Detected, status.Path); err != nil {
				return err
			}
		}
		return nil
	case domain.Remote:
		_, err := fmt.Fprintf(writer, "%s\t%s\n", value.Name, value.Path)
		return err
	case []domain.Remote:
		for _, remote := range value {
			if _, err := fmt.Fprintf(writer, "%s\t%s\n", remote.Name, remote.Path); err != nil {
				return err
			}
		}
		return nil
	case domain.RemoteSyncResult:
		_, err := fmt.Fprintf(writer, "%s\t%s\t%s\tlocal:%s\tremote:%s\tobjects:%d\n", value.RemoteName, value.RefName, value.Outcome, value.LocalTip, value.RemoteTip, value.TransferredObjects)
		return err
	case app.CandidateImportResult:
		_, err := fmt.Fprintf(writer, "%s\t%s\timported:%t\tdrafts:%d\n", value.Candidate.ID, value.Candidate.State, value.Imported, value.DraftNotes)
		return err
	case []domain.Candidate:
		for _, candidate := range value {
			if _, err := fmt.Fprintf(writer, "%s\t%s\t%s\t%s\n", candidate.ID, candidate.State, candidate.HeadSHA, candidate.UpdatedAt.Format(time.RFC3339)); err != nil {
				return err
			}
		}
		return nil
	case app.CandidateResult:
		if _, err := fmt.Fprintf(writer, "%s\t%s\tbase:%s\thead:%s\tmerge:%s\n", value.Candidate.ID, value.Candidate.State, value.Candidate.BaseSHA, value.Candidate.HeadSHA, value.Candidate.MergeSHA); err != nil {
			return err
		}
		for _, note := range value.Notes {
			if _, err := fmt.Fprintf(writer, "%s\t%s\t%s\t%s\n", note.State, note.ID, note.EntityKey, note.Body); err != nil {
				return err
			}
		}
		return nil
	case domain.CandidatePromotionResult:
		_, err := fmt.Fprintf(writer, "%s\tpromoted:%t\tactive:%d\tneeds-review:%d\thistorical:%d\n", value.CandidateID, value.Promoted, value.ActiveNotes, value.NeedsReviewNotes, value.HistoricalNotes)
		return err
	case domain.LandingIntent:
		_, err := fmt.Fprintf(writer, "%s\t%s\t%s\tmerge:%s\tcontext:%s\n", value.ID, value.State, value.TargetRef, value.SourceMergeSHA, value.LandedContextCommitID)
		return err
	case []domain.LandingIntent:
		for _, landing := range value {
			if _, err := fmt.Fprintf(writer, "%s\t%s\t%s\tmerge:%s\n", landing.ID, landing.State, landing.TargetRef, landing.SourceMergeSHA); err != nil {
				return err
			}
		}
		return nil
	case domain.LandingSession:
		_, err := fmt.Fprintf(writer, "%s\t%s\tlanding:%s\tconflicts:%d\tplan:%s\n", value.ID, value.State, value.LandingID, len(value.Plan.Conflicts), value.Plan.ID)
		return err
	case app.ContextResult:
		if _, err := fmt.Fprintf(writer, "%s\t%s\t%s\n", value.Entity.Key, value.Entity.Kind, value.Entity.Path); err != nil {
			return err
		}
		for _, note := range value.Notes {
			state := "committed"
			if note.Pending {
				state = "pending"
			}
			if _, err := fmt.Fprintf(writer, "[%s] %s\t%s\n", state, note.Kind, note.Body); err != nil {
				return err
			}
		}
		return nil
	case domain.Note:
		_, err := fmt.Fprintf(writer, "pending note %s for %s\n", value.ID, value.EntityKey)
		return err
	case domain.ContextCommit:
		_, err := fmt.Fprintf(writer, "context commit %s\n", value.ID)
		return err
	case domain.MergeSession:
		_, err := fmt.Fprintf(writer, "%s\t%s\tconflicts:%d\tautomatic:%d\n", value.ID, value.State, len(value.Conflicts), len(value.AutomaticRecords))
		return err
	case []domain.SearchHit:
		for _, hit := range value {
			if _, err := fmt.Fprintf(writer, "%s\t%s\tfields:%s\tterms:%s\tnotes:%s\tbinding:%s\tfresh:%t\t%s\n", hit.EntityKey, hit.Path, strings.Join(searchMatchFields(hit.MatchedFields), ","), strings.Join(hit.MatchedTerms, ","), strings.Join(hit.NoteIDs, ","), hit.BindingState, hit.Fresh, hit.Snippet); err != nil {
				return err
			}
		}
		return nil
	case []domain.RelatedEntity:
		for _, entity := range value {
			if _, err := fmt.Fprintf(writer, "%s\t%s\t%s\n", entity.EdgeKind, entity.EntityKey, entity.Path); err != nil {
				return err
			}
		}
		return nil
	case domain.ContextBundle:
		if _, err := fmt.Fprintf(writer, "source: %s\tcomplete:%t\n", value.Source.SourceSHA, value.Complete); err != nil {
			return err
		}
		for _, item := range value.Items {
			reasons := make([]string, len(item.Reasons))
			for index, reason := range item.Reasons {
				reasons[index] = string(reason.Kind)
			}
			if _, err := fmt.Fprintf(writer, "%s\t%s\t%s\treasons:%s\thistorical:%t\n", item.Note.Kind, item.BoundEntity.EntityKey, item.Note.Body, strings.Join(reasons, ","), item.Historical); err != nil {
				return err
			}
		}
		return nil
	case []domain.Note:
		for _, note := range value {
			state := note.BindingState
			if state == "" {
				state = domain.NoteBindingActive
			}
			if _, err := fmt.Fprintf(writer, "%s\t%s\t%s\t%s\n", state, note.Kind, note.EntityKey, note.Body); err != nil {
				return err
			}
		}
		return nil
	case []domain.ContextCommit:
		for _, commit := range value {
			if _, err := fmt.Fprintf(writer, "%s\t%s\n", commit.ID, commit.Message); err != nil {
				return err
			}
		}
		return nil
	default:
		_, err := fmt.Fprintln(writer, "ok")
		return err
	}
}

func searchMatchFields(fields []domain.SearchMatchField) []string {
	values := make([]string, len(fields))
	for index, field := range fields {
		values[index] = string(field)
	}
	return values
}

func (r *Runner) writeError(writer io.Writer, jsonOutput bool, err error) {
	code := domain.CodeOf(err)
	if code == "" {
		code = domain.CodeLocalStorage
	}
	if jsonOutput {
		_ = json.NewEncoder(writer).Encode(struct {
			Version int `json:"version"`
			Error   struct {
				Code    domain.ErrorCode `json:"code"`
				Message string           `json:"message"`
			} `json:"error"`
		}{
			Version: OutputVersion,
			Error: struct {
				Code    domain.ErrorCode `json:"code"`
				Message string           `json:"message"`
			}{Code: code, Message: err.Error()},
		})
		return
	}
	_, _ = fmt.Fprintln(writer, "error:", err)
}

func exitCode(err error) int {
	switch domain.CodeOf(err) {
	case domain.CodeValidation:
		return 2
	case domain.CodeRepositoryState:
		return 3
	case domain.CodeNotInitialized:
		return 4
	case domain.CodeNothingToCommit, domain.CodeStaleWorkingSet, domain.CodeWorkingSetDirty, domain.CodeCoverageIncomplete:
		return 5
	case domain.CodeConcurrentUpdate, domain.CodeBusy:
		return 6
	case domain.CodeRemoteConflict:
		return 6
	case domain.CodeAuth:
		return 8
	default:
		return 7
	}
}

func noArgs(_ *cobra.Command, arguments []string) error {
	return exactArgs(0)(nil, arguments)
}

func exactArgs(expected int) cobra.PositionalArgs {
	return func(_ *cobra.Command, arguments []string) error {
		if len(arguments) != expected {
			return domain.NewError(domain.CodeValidation, fmt.Errorf("accepts %d argument(s), received %d", expected, len(arguments)))
		}
		return nil
	}
}
