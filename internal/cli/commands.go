package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/tae2089/thread-keep/internal/app"
	"github.com/tae2089/thread-keep/internal/domain"
)

func Commands(runner *Runner) []*cobra.Command {
	return []*cobra.Command{
		initCommand(runner),
		indexersCommand(runner),
		remoteCommand(runner),
		candidateCommand(runner),
		landingCommand(runner),
		updateCommand(runner),
		statusCommand(runner),
		searchCommand(runner),
		contextCommand(runner),
		noteCommand(runner),
		diffCommand(runner),
		commitCommand(runner),
		logCommand(runner),
		rebuildCommand(runner),
	}
}

func landingCommand(runner *Runner) *cobra.Command {
	command := &cobra.Command{Use: "landing", Short: "inspect and recover PR context landings", Args: noArgs, RunE: requireSubcommand("landing")}
	command.AddCommand(&cobra.Command{Use: "list <remote>", Short: "list landing intents", Args: exactArgs(1), RunE: runner.withService(func(ctx context.Context, service *app.Service, command *cobra.Command, arguments []string) (any, error) {
		return service.Landings(ctx, arguments[0])
	})})
	command.AddCommand(&cobra.Command{Use: "show <remote> <landing-id>", Short: "show one landing intent", Args: exactArgs(2), RunE: runner.withService(func(ctx context.Context, service *app.Service, command *cobra.Command, arguments []string) (any, error) {
		return service.Landing(ctx, arguments[0], arguments[1])
	})})
	command.AddCommand(&cobra.Command{Use: "recover <remote> <landing-id>", Short: "start local recovery at the exact merged source", Args: exactArgs(2), RunE: runner.withService(func(ctx context.Context, service *app.Service, command *cobra.Command, arguments []string) (any, error) {
		return service.RecoverLanding(ctx, arguments[0], arguments[1])
	})})
	session := &cobra.Command{Use: "session", Short: "inspect local landing recovery sessions", Args: noArgs, RunE: requireSubcommand("landing session")}
	session.AddCommand(&cobra.Command{Use: "show <session-id>", Short: "show one local landing recovery session", Args: exactArgs(1), RunE: runner.withService(func(ctx context.Context, service *app.Service, command *cobra.Command, arguments []string) (any, error) {
		return service.LandingSession(ctx, arguments[0])
	})})
	command.AddCommand(session)
	resolve := &cobra.Command{Use: "resolve <session-id> <conflict-id>", Short: "resolve one landing conflict", Args: exactArgs(2), RunE: runner.withService(func(ctx context.Context, service *app.Service, command *cobra.Command, arguments []string) (any, error) {
		use, err := command.Flags().GetString("use")
		if err != nil {
			return nil, err
		}
		file, err := command.Flags().GetString("file")
		if err != nil {
			return nil, err
		}
		var authored *domain.Note
		if strings.TrimSpace(file) != "" {
			contents, err := os.ReadFile(file)
			if err != nil {
				return nil, domain.NewError(domain.CodeValidation, fmt.Errorf("read authored landing note: %w", err))
			}
			var note domain.Note
			if err := json.Unmarshal(contents, &note); err != nil {
				return nil, domain.NewError(domain.CodeValidation, errors.New("authored landing note file must contain one valid note JSON value"))
			}
			authored = &note
		}
		return service.ResolveLanding(ctx, app.LandingResolveInput{SessionID: arguments[0], ConflictID: arguments[1], Use: use, Authored: authored})
	})}
	resolve.Flags().String("use", "", "resolution: canonical, candidate, or authored")
	resolve.Flags().String("file", "", "authored note JSON file (required with --use authored)")
	command.AddCommand(resolve)
	commit := &cobra.Command{Use: "commit <session-id>", Short: "commit one ready landing recovery", Args: exactArgs(1), RunE: runner.withService(func(ctx context.Context, service *app.Service, command *cobra.Command, arguments []string) (any, error) {
		message, err := command.Flags().GetString("message")
		if err != nil {
			return nil, err
		}
		author, err := command.Flags().GetString("author")
		if err != nil {
			return nil, err
		}
		return service.CommitLanding(ctx, app.LandingCommitInput{SessionID: arguments[0], Message: message, Author: author})
	})}
	commit.Flags().StringP("message", "m", "", "landing context commit message")
	commit.Flags().String("author", "", "landing context author")
	command.AddCommand(commit)
	return command
}

func candidateCommand(runner *Runner) *cobra.Command {
	command := &cobra.Command{Use: "candidate", Short: "import and promote provider-neutral context candidates", Args: noArgs, RunE: requireSubcommand("candidate")}
	command.AddCommand(&cobra.Command{Use: "import <file>", Short: "import one local candidate envelope", Args: exactArgs(1), RunE: runner.withService(func(ctx context.Context, service *app.Service, command *cobra.Command, arguments []string) (any, error) {
		return service.ImportCandidate(ctx, arguments[0])
	})})
	command.AddCommand(&cobra.Command{Use: "list", Short: "list imported candidates", Args: noArgs, RunE: runner.withService(func(ctx context.Context, service *app.Service, command *cobra.Command, _ []string) (any, error) {
		return service.Candidates(ctx)
	})})
	command.AddCommand(&cobra.Command{Use: "show <candidate-id>", Short: "show one candidate and its draft notes", Args: exactArgs(1), RunE: runner.withService(func(ctx context.Context, service *app.Service, command *cobra.Command, arguments []string) (any, error) {
		return service.Candidate(ctx, arguments[0])
	})})
	command.AddCommand(&cobra.Command{Use: "promote <candidate-id>", Short: "explicitly promote one merged candidate", Args: exactArgs(1), RunE: runner.withService(func(ctx context.Context, service *app.Service, command *cobra.Command, arguments []string) (any, error) {
		return service.PromoteCandidate(ctx, arguments[0])
	})})
	publish := &cobra.Command{Use: "publish <remote>", Short: "publish the current PR context delta", Args: exactArgs(1), RunE: runner.withService(func(ctx context.Context, service *app.Service, command *cobra.Command, arguments []string) (any, error) {
		change, err := command.Flags().GetString("change")
		if err != nil {
			return nil, err
		}
		return service.PublishCandidate(ctx, arguments[0], change)
	})}
	publish.Flags().String("change", "", "provider change key, for example github:owner/repository#42")
	command.AddCommand(publish)
	return command
}

func remoteCommand(runner *Runner) *cobra.Command {
	command := &cobra.Command{Use: "remote", Short: "configure and synchronize immutable context remotes", Args: noArgs, RunE: requireSubcommand("remote")}
	command.AddCommand(&cobra.Command{Use: "add <name> <path>", Short: "add one filesystem remote", Args: exactArgs(2), RunE: runner.withService(func(ctx context.Context, service *app.Service, command *cobra.Command, arguments []string) (any, error) {
		return service.AddRemote(ctx, arguments[0], arguments[1])
	})})
	command.AddCommand(&cobra.Command{Use: "list", Short: "list configured filesystem remotes", Args: noArgs, RunE: runner.withService(func(ctx context.Context, service *app.Service, command *cobra.Command, _ []string) (any, error) {
		return service.Remotes(ctx)
	})})
	command.AddCommand(&cobra.Command{Use: "push <name>", Short: "push immutable context objects and ref", Args: exactArgs(1), RunE: runner.withService(func(ctx context.Context, service *app.Service, command *cobra.Command, arguments []string) (any, error) {
		return service.PushRemote(ctx, arguments[0])
	})})
	command.AddCommand(&cobra.Command{Use: "fetch <name>", Short: "fetch immutable context objects and tracking ref", Args: exactArgs(1), RunE: runner.withService(func(ctx context.Context, service *app.Service, command *cobra.Command, arguments []string) (any, error) {
		return service.FetchRemote(ctx, arguments[0])
	})})
	command.AddCommand(&cobra.Command{Use: "pull <name>", Short: "fast-forward local context from one filesystem remote", Args: exactArgs(1), RunE: runner.withService(func(ctx context.Context, service *app.Service, command *cobra.Command, arguments []string) (any, error) {
		return service.PullRemote(ctx, arguments[0])
	})})
	return command
}

func initCommand(runner *Runner) *cobra.Command {
	return &cobra.Command{Use: "init", Short: "initialize local context storage", Args: noArgs, RunE: runner.withService(func(ctx context.Context, service *app.Service, command *cobra.Command, _ []string) (any, error) {
		if err := service.Init(ctx); err != nil {
			return nil, err
		}
		return map[string]bool{"initialized": true}, nil
	})}
}

func indexersCommand(runner *Runner) *cobra.Command {
	command := &cobra.Command{Use: "indexers", Short: "inspect known language indexers", Args: noArgs, RunE: requireSubcommand("indexers")}
	command.AddCommand(&cobra.Command{Use: "list", Short: "list builtin and locally installed indexers", Args: noArgs, RunE: runner.withService(func(ctx context.Context, service *app.Service, command *cobra.Command, _ []string) (any, error) {
		return service.Indexers(ctx)
	})})
	install := &cobra.Command{Use: "install", Short: "install detected missing official indexers", Args: noArgs, RunE: runner.withService(func(ctx context.Context, service *app.Service, command *cobra.Command, _ []string) (any, error) {
		detected, err := command.Flags().GetBool("detected")
		if err != nil {
			return nil, err
		}
		if !detected {
			return nil, domain.NewError(domain.CodeValidation, errors.New("indexers install requires --detected"))
		}
		return service.InstallIndexers(ctx)
	})}
	install.Flags().Bool("detected", false, "install only official packs detected in the current repository")
	command.AddCommand(install)
	syncCommand := &cobra.Command{Use: "sync", Short: "activate detected official indexers from a signed release", Args: noArgs, RunE: runner.withService(func(ctx context.Context, service *app.Service, command *cobra.Command, _ []string) (any, error) {
		detected, err := command.Flags().GetBool("detected")
		if err != nil {
			return nil, err
		}
		if !detected {
			return nil, domain.NewError(domain.CodeValidation, errors.New("indexers sync requires --detected"))
		}
		version, err := command.Flags().GetString("version")
		if err != nil {
			return nil, err
		}
		return service.SyncIndexers(ctx, version)
	})}
	syncCommand.Flags().Bool("detected", false, "sync only official packs detected in the current repository")
	syncCommand.Flags().String("version", "", "activate an exact stable release version (X.Y.Z); defaults to latest")
	command.AddCommand(syncCommand)
	return command
}

func updateCommand(runner *Runner) *cobra.Command {
	command := &cobra.Command{Use: "update", Short: "index supported languages in the current worktree", Args: noArgs, RunE: runner.withService(func(ctx context.Context, service *app.Service, command *cobra.Command, _ []string) (any, error) {
		requireComplete, err := command.Flags().GetBool("require-complete")
		if err != nil {
			return nil, err
		}
		return service.UpdateWithOptions(ctx, requireComplete)
	})}
	command.Flags().Bool("require-complete", false, "fail if any detected language lacks fresh indexing coverage")
	return command
}

func statusCommand(runner *Runner) *cobra.Command {
	return &cobra.Command{Use: "status", Short: "show working context status", Args: noArgs, RunE: runner.withService(func(ctx context.Context, service *app.Service, command *cobra.Command, _ []string) (any, error) {
		return service.Status(ctx)
	})}
}

func searchCommand(runner *Runner) *cobra.Command {
	return &cobra.Command{Use: "search <query>", Short: "search indexed entities and context notes", Args: exactArgs(1), RunE: runner.withService(func(ctx context.Context, service *app.Service, command *cobra.Command, arguments []string) (any, error) {
		return service.Search(ctx, arguments[0])
	})}
}

func contextCommand(runner *Runner) *cobra.Command {
	command := &cobra.Command{Use: "context", Short: "read entity context", Args: noArgs, RunE: requireSubcommand("context")}
	command.AddCommand(&cobra.Command{Use: "get <entity-key>", Short: "show one entity and its notes", Args: exactArgs(1), RunE: runner.withService(func(ctx context.Context, service *app.Service, command *cobra.Command, arguments []string) (any, error) {
		return service.Context(ctx, arguments[0])
	})})
	related := &cobra.Command{Use: "related <entity-key>", Short: "show bounded structural context for one entity", Args: exactArgs(1), RunE: runner.withService(func(ctx context.Context, service *app.Service, command *cobra.Command, arguments []string) (any, error) {
		limit, err := command.Flags().GetInt("limit")
		if err != nil {
			return nil, err
		}
		return service.RelatedContext(ctx, arguments[0], limit)
	})}
	related.Flags().Int("limit", 20, "maximum related entities (1-100)")
	command.AddCommand(related)
	forEntity := &cobra.Command{Use: "for-entity <entity-key>", Short: "assemble evidence-backed context for one entity", Args: exactArgs(1), RunE: runner.withService(func(ctx context.Context, service *app.Service, command *cobra.Command, arguments []string) (any, error) {
		query, err := contextQueryFromFlags(command, domain.ContextAnchor{Kind: domain.AnchorEntity, EntityKey: arguments[0]}, true)
		if err != nil {
			return nil, err
		}
		return service.AssembleContext(ctx, query)
	})}
	addContextQueryFlags(forEntity, true)
	command.AddCommand(forEntity)
	forChange := &cobra.Command{Use: "for-change", Short: "assemble evidence-backed context for entity changes since a context snapshot", Args: noArgs, RunE: runner.withService(func(ctx context.Context, service *app.Service, command *cobra.Command, _ []string) (any, error) {
		baseContextID, err := command.Flags().GetString("base-context")
		if err != nil {
			return nil, err
		}
		query, err := contextQueryFromFlags(command, domain.ContextAnchor{Kind: domain.AnchorChange, BaseContextID: baseContextID}, true)
		if err != nil {
			return nil, err
		}
		return service.AssembleContext(ctx, query)
	})}
	addContextQueryFlags(forChange, true)
	forChange.Flags().String("base-context", "", "immutable base context snapshot ID (default current context tip)")
	command.AddCommand(forChange)
	query := &cobra.Command{Use: "query <text>", Short: "assemble evidence-backed context from lexical evidence", Args: exactArgs(1), RunE: runner.withService(func(ctx context.Context, service *app.Service, command *cobra.Command, arguments []string) (any, error) {
		input, err := contextQueryFromFlags(command, domain.ContextAnchor{Kind: domain.AnchorText, Query: arguments[0]}, false)
		if err != nil {
			return nil, err
		}
		return service.AssembleContext(ctx, input)
	})}
	addContextQueryFlags(query, false)
	command.AddCommand(query)
	merge := &cobra.Command{Use: "merge", Short: "resolve an explicit semantic context merge", Args: noArgs, RunE: requireSubcommand("context merge")}
	start := &cobra.Command{Use: "start <local-snapshot-id> <remote-snapshot-id>", Short: "start one explicit semantic merge session", Args: exactArgs(2), RunE: runner.withService(func(ctx context.Context, service *app.Service, command *cobra.Command, arguments []string) (any, error) {
		message, err := command.Flags().GetString("message")
		if err != nil {
			return nil, err
		}
		author, err := command.Flags().GetString("author")
		if err != nil {
			return nil, err
		}
		return service.StartMerge(ctx, app.MergeStartInput{LocalSnapshotID: arguments[0], RemoteSnapshotID: arguments[1], Message: message, Author: author})
	})}
	start.Flags().StringP("message", "m", "", "merge context message")
	start.Flags().String("author", "", "merge author")
	merge.AddCommand(start)
	merge.AddCommand(&cobra.Command{Use: "show <session-id>", Short: "show one semantic merge session", Args: exactArgs(1), RunE: runner.withService(func(ctx context.Context, service *app.Service, command *cobra.Command, arguments []string) (any, error) {
		return service.MergeSession(ctx, arguments[0])
	})})
	resolve := &cobra.Command{Use: "resolve <session-id> <conflict-id>", Short: "resolve one semantic merge conflict", Args: exactArgs(2), RunE: runner.withService(func(ctx context.Context, service *app.Service, command *cobra.Command, arguments []string) (any, error) {
		use, err := command.Flags().GetString("use")
		if err != nil {
			return nil, err
		}
		entityKey, err := command.Flags().GetString("entity")
		if err != nil {
			return nil, err
		}
		kind, err := command.Flags().GetString("kind")
		if err != nil {
			return nil, err
		}
		body, err := command.Flags().GetString("body")
		if err != nil {
			return nil, err
		}
		author, err := command.Flags().GetString("author")
		if err != nil {
			return nil, err
		}
		origin, err := command.Flags().GetString("origin")
		if err != nil {
			return nil, err
		}
		return service.ResolveMerge(ctx, app.MergeResolveInput{SessionID: arguments[0], ConflictID: arguments[1], Use: use, EntityKey: entityKey, Kind: kind, Body: body, Author: author, Origin: origin})
	})}
	resolve.Flags().String("use", "", "resolution: local, remote, or authored")
	resolve.Flags().String("entity", "", "authored entity key")
	resolve.Flags().String("kind", "", "authored note kind")
	resolve.Flags().String("body", "", "authored note body")
	resolve.Flags().String("author", "", "authored note author")
	resolve.Flags().String("origin", "human", "authored note origin")
	merge.AddCommand(resolve)
	merge.AddCommand(&cobra.Command{Use: "commit <session-id>", Short: "commit one resolved semantic merge", Args: exactArgs(1), RunE: runner.withService(func(ctx context.Context, service *app.Service, command *cobra.Command, arguments []string) (any, error) {
		return service.CommitMerge(ctx, arguments[0])
	})})
	command.AddCommand(merge)
	return command
}

func addContextQueryFlags(command *cobra.Command, narrowingText bool) {
	command.Flags().StringSlice("kind", nil, "note kinds to include")
	command.Flags().StringSlice("state", nil, "binding states to include (default active)")
	command.Flags().StringSlice("language", nil, "entity languages to include")
	command.Flags().StringSlice("path", nil, "repository-relative path prefixes to include")
	command.Flags().StringSlice("entity-kind", nil, "entity kinds to include")
	command.Flags().StringSlice("topic", nil, "exact note topics to include")
	command.Flags().String("history", "current", "history mode: current")
	command.Flags().Int("limit", 20, "maximum context items (1-100)")
	if narrowingText {
		command.Flags().String("query", "", "optional lexical narrowing text")
	}
}

func contextQueryFromFlags(command *cobra.Command, anchor domain.ContextAnchor, narrowingText bool) (domain.ContextQuery, error) {
	kinds, err := command.Flags().GetStringSlice("kind")
	if err != nil {
		return domain.ContextQuery{}, err
	}
	states, err := command.Flags().GetStringSlice("state")
	if err != nil {
		return domain.ContextQuery{}, err
	}
	languages, err := command.Flags().GetStringSlice("language")
	if err != nil {
		return domain.ContextQuery{}, err
	}
	paths, err := command.Flags().GetStringSlice("path")
	if err != nil {
		return domain.ContextQuery{}, err
	}
	entityKinds, err := command.Flags().GetStringSlice("entity-kind")
	if err != nil {
		return domain.ContextQuery{}, err
	}
	topics, err := command.Flags().GetStringSlice("topic")
	if err != nil {
		return domain.ContextQuery{}, err
	}
	history, err := command.Flags().GetString("history")
	if err != nil {
		return domain.ContextQuery{}, err
	}
	limit, err := command.Flags().GetInt("limit")
	if err != nil {
		return domain.ContextQuery{}, err
	}
	var text string
	if narrowingText {
		text, err = command.Flags().GetString("query")
		if err != nil {
			return domain.ContextQuery{}, err
		}
	}
	return domain.ContextQuery{
		Anchor: anchor, Text: text, Kinds: noteKinds(kinds), States: bindingStates(states),
		Languages: cleanStrings(languages), Paths: cleanStrings(paths), EntityKinds: entityKindsFromStrings(entityKinds),
		Topics: cleanStrings(topics), History: domain.HistoryMode(strings.TrimSpace(history)), Limit: limit,
	}, nil
}

func noteKinds(values []string) []domain.NoteKind {
	result := make([]domain.NoteKind, 0, len(values))
	for _, value := range cleanStrings(values) {
		result = append(result, domain.NoteKind(value))
	}
	return result
}

func bindingStates(values []string) []domain.NoteBindingState {
	result := make([]domain.NoteBindingState, 0, len(values))
	for _, value := range cleanStrings(values) {
		result = append(result, domain.NoteBindingState(value))
	}
	return result
}

func entityKindsFromStrings(values []string) []domain.EntityKind {
	result := make([]domain.EntityKind, 0, len(values))
	for _, value := range cleanStrings(values) {
		result = append(result, domain.EntityKind(value))
	}
	return result
}

func cleanStrings(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			result = append(result, value)
		}
	}
	return result
}

func noteCommand(runner *Runner) *cobra.Command {
	command := &cobra.Command{Use: "note", Short: "manage pending context notes", Args: noArgs, RunE: requireSubcommand("note")}
	add := &cobra.Command{Use: "add <entity-key>", Short: "add a pending context note", Args: exactArgs(1), RunE: runner.withService(func(ctx context.Context, service *app.Service, command *cobra.Command, arguments []string) (any, error) {
		kind, err := command.Flags().GetString("kind")
		if err != nil {
			return nil, err
		}
		body, err := command.Flags().GetString("body")
		if err != nil {
			return nil, err
		}
		author, err := command.Flags().GetString("author")
		if err != nil {
			return nil, err
		}
		origin, err := command.Flags().GetString("origin")
		if err != nil {
			return nil, err
		}
		topics, err := command.Flags().GetStringSlice("topic")
		if err != nil {
			return nil, err
		}
		return service.AddNote(ctx, app.AddNoteInput{EntityKey: arguments[0], Kind: kind, Body: body, Author: author, Origin: origin, Topics: topics})
	})}
	add.Flags().String("kind", "", "note kind: intent, decision, constraint, example or warning")
	add.Flags().String("body", "", "note body")
	add.Flags().String("author", "", "note author")
	add.Flags().String("origin", "human", "note origin")
	add.Flags().StringSlice("topic", nil, "immutable note topic (repeatable)")
	command.AddCommand(add)
	revise := &cobra.Command{Use: "revise <note-id>", Short: "create an immutable revision of a committed note", Args: exactArgs(1), RunE: runner.withService(func(ctx context.Context, service *app.Service, command *cobra.Command, arguments []string) (any, error) {
		body, err := command.Flags().GetString("body")
		if err != nil {
			return nil, err
		}
		author, err := command.Flags().GetString("author")
		if err != nil {
			return nil, err
		}
		origin, err := command.Flags().GetString("origin")
		if err != nil {
			return nil, err
		}
		topics, err := command.Flags().GetStringSlice("topic")
		if err != nil {
			return nil, err
		}
		return service.ReviseNote(ctx, app.ReviseNoteInput{NoteID: arguments[0], Body: body, Author: author, Origin: origin, Topics: topics})
	})}
	revise.Flags().String("body", "", "replacement note body")
	revise.Flags().String("author", "", "note revision author")
	revise.Flags().String("origin", "human", "note revision origin")
	revise.Flags().StringSlice("topic", nil, "replacement immutable topics (repeatable)")
	command.AddCommand(revise)
	review := &cobra.Command{Use: "review <note-id>", Short: "confirm a needs-review note binding", Args: exactArgs(1), RunE: runner.withService(func(ctx context.Context, service *app.Service, command *cobra.Command, arguments []string) (any, error) {
		entityKey, err := command.Flags().GetString("entity")
		if err != nil {
			return nil, err
		}
		return service.ReviewNote(ctx, app.ReviewNoteInput{NoteID: arguments[0], EntityKey: entityKey})
	})}
	review.Flags().String("entity", "", "current entity key to confirm")
	command.AddCommand(review)
	return command
}

func requireSubcommand(name string) func(*cobra.Command, []string) error {
	return func(_ *cobra.Command, _ []string) error {
		return domain.NewError(domain.CodeValidation, fmt.Errorf("%s requires a subcommand", name))
	}
}

func diffCommand(runner *Runner) *cobra.Command {
	return &cobra.Command{Use: "diff", Short: "show pending context changes", Args: noArgs, RunE: runner.withService(func(ctx context.Context, service *app.Service, command *cobra.Command, _ []string) (any, error) {
		return service.Diff(ctx)
	})}
}

func commitCommand(runner *Runner) *cobra.Command {
	command := &cobra.Command{Use: "commit", Short: "commit pending context changes", Args: noArgs, RunE: runner.withService(func(ctx context.Context, service *app.Service, command *cobra.Command, _ []string) (any, error) {
		message, err := command.Flags().GetString("message")
		if err != nil {
			return nil, err
		}
		author, err := command.Flags().GetString("author")
		if err != nil {
			return nil, err
		}
		return service.Commit(ctx, app.CommitInput{Message: message, Author: author})
	})}
	command.Flags().StringP("message", "m", "", "context commit message")
	command.Flags().String("author", "", "context commit author")
	return command
}

func logCommand(runner *Runner) *cobra.Command {
	command := &cobra.Command{Use: "log", Short: "show context commit history", Args: noArgs, RunE: runner.withService(func(ctx context.Context, service *app.Service, command *cobra.Command, _ []string) (any, error) {
		limit, err := command.Flags().GetInt("limit")
		if err != nil {
			return nil, err
		}
		return service.Log(ctx, limit)
	})}
	command.Flags().Int("limit", 20, "maximum commits to show")
	return command
}

func rebuildCommand(runner *Runner) *cobra.Command {
	return &cobra.Command{Use: "rebuild <context-commit-id>", Short: "rebuild an empty local projection from an immutable context commit", Args: exactArgs(1), RunE: runner.withService(func(ctx context.Context, service *app.Service, command *cobra.Command, arguments []string) (any, error) {
		return service.Rebuild(ctx, arguments[0])
	})}
}
