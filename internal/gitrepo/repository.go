package gitrepo

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tae2089/thread-keep/internal/domain"
)

type State struct {
	Root         string
	CommonDir    string
	RepositoryID string
	WorktreeID   string
	Branch       string
	HeadSHA      string
}

func Discover(ctx context.Context, cwd string) (State, error) {
	root, err := run(ctx, cwd, "rev-parse", "--show-toplevel")
	if err != nil {
		return State{}, domain.NewError(domain.CodeRepositoryState, errors.New("not inside a Git worktree"))
	}
	commonDir, err := run(ctx, root, "rev-parse", "--git-common-dir")
	if err != nil {
		return State{}, domain.NewError(domain.CodeRepositoryState, fmt.Errorf("resolve Git common directory: %w", err))
	}
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(root, commonDir)
	}
	commonDir, err = filepath.Abs(commonDir)
	if err != nil {
		return State{}, domain.NewError(domain.CodeRepositoryState, fmt.Errorf("normalize Git common directory: %w", err))
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return State{}, domain.NewError(domain.CodeRepositoryState, fmt.Errorf("normalize Git root: %w", err))
	}

	state := State{Root: root, CommonDir: commonDir, RepositoryID: repositoryIdentity(ctx, root, commonDir), WorktreeID: root}
	if branch, branchErr := run(ctx, root, "symbolic-ref", "--quiet", "--short", "HEAD"); branchErr == nil {
		state.Branch = branch
	}
	if head, headErr := run(ctx, root, "rev-parse", "--verify", "HEAD"); headErr == nil {
		state.HeadSHA = head
	}
	return state, nil
}

func repositoryIdentity(ctx context.Context, root, fallback string) string {
	roots, err := run(ctx, root, "rev-list", "--max-parents=0", "HEAD")
	if err != nil || roots == "" {
		return fallback
	}
	identifiers := strings.Fields(roots)
	sort.Strings(identifiers)
	return "git-roots:" + strings.Join(identifiers, ",")
}

func (s State) RequireMutableState() error {
	if s.Branch == "" {
		return domain.NewError(domain.CodeRepositoryState, errors.New("detached HEAD is not supported for state-changing commands"))
	}
	if s.HeadSHA == "" {
		return domain.NewError(domain.CodeRepositoryState, errors.New("unborn HEAD is not supported for state-changing commands"))
	}
	return nil
}

func (s State) RequireCleanWorktree(ctx context.Context) error {
	dirty, err := s.HasUncommittedChanges(ctx)
	if err != nil {
		return err
	}
	if dirty {
		return domain.NewError(domain.CodeRepositoryState, errors.New("working tree must be clean before indexing context"))
	}
	return nil
}

func (s State) HasUncommittedChanges(ctx context.Context) (bool, error) {
	status, err := run(ctx, s.Root, "status", "--porcelain")
	if err != nil {
		return false, domain.NewError(domain.CodeRepositoryState, fmt.Errorf("inspect Git worktree state: %w", err))
	}
	return status != "", nil
}

func (s State) IsAncestor(ctx context.Context, ancestor, descendant string) (bool, error) {
	if strings.TrimSpace(ancestor) == "" || strings.TrimSpace(descendant) == "" {
		return false, domain.NewError(domain.CodeValidation, errors.New("Git ancestry requires two source revisions"))
	}
	command := exec.CommandContext(ctx, "git", "-C", s.Root, "merge-base", "--is-ancestor", ancestor, descendant)
	if err := command.Run(); err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) && exitError.ExitCode() == 1 {
			return false, nil
		}
		return false, domain.NewError(domain.CodeRepositoryState, fmt.Errorf("inspect Git source ancestry: %w", err))
	}
	return true, nil
}

func (s State) UserName(ctx context.Context) string {
	name, err := run(ctx, s.Root, "config", "--get", "user.name")
	if err != nil {
		return ""
	}
	return name
}

func (s State) RefName() string {
	return "refs/contexts/" + s.Branch
}

func run(ctx context.Context, cwd string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, "git", append([]string{"-C", cwd}, args...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
}
