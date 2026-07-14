package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/tae2089/thread-keep/internal/domain"
)

const (
	pythonExecutableEnv = "THREAD_KEEP_PYTHON_EXECUTABLE"
	packageVersionEnv   = "THREAD_KEEP_PACKAGE_VERSION"
)

var packLanguages = []string{"typescript", "javascript", "python", "java", "kotlin", "rust"}

type PackInstallResult struct {
	Languages   []string `json:"languages"`
	Requirement string   `json:"requirement"`
}

func packCommand(runner *Runner) *cobra.Command {
	command := &cobra.Command{Use: "pack", Short: "install optional language packs from PyPI", Args: noArgs, RunE: requireSubcommand("pack")}
	command.AddCommand(&cobra.Command{
		Use:   "install <language>...",
		Short: "install selected language packs into the current Python environment",
		Args: func(_ *cobra.Command, arguments []string) error {
			if len(arguments) == 0 {
				return domain.NewError(domain.CodeValidation, errors.New("pack install requires at least one language"))
			}
			return nil
		},
		RunE: func(command *cobra.Command, arguments []string) error {
			result, err := runner.installPacks(command, arguments)
			if err != nil {
				return err
			}
			jsonOutput, err := command.Root().PersistentFlags().GetBool("json")
			if err != nil {
				return err
			}
			return writeResult(command.OutOrStdout(), jsonOutput, result)
		},
	})
	return command
}

func (r *Runner) installPacks(command *cobra.Command, arguments []string) (PackInstallResult, error) {
	languages, err := normalizePackLanguages(arguments)
	if err != nil {
		return PackInstallResult{}, err
	}
	python := strings.TrimSpace(os.Getenv(pythonExecutableEnv))
	version := strings.TrimSpace(os.Getenv(packageVersionEnv))
	if !filepath.IsAbs(python) || !validPackageVersion(version) {
		return PackInstallResult{}, domain.NewError(domain.CodeValidation, errors.New("pack install requires thread-keep to be launched from its PyPI package"))
	}
	if info, err := os.Stat(python); err != nil || !info.Mode().IsRegular() {
		return PackInstallResult{}, domain.NewError(domain.CodeValidation, errors.New("PyPI Python executable is unavailable"))
	}
	requirement := fmt.Sprintf("thread-keep[%s]==%s", strings.Join(languages, ","), version)
	process := r.runProcess
	if process == nil {
		process = runProcess
	}
	stdout := command.OutOrStdout()
	stderr := command.ErrOrStderr()
	jsonOutput, err := command.Root().PersistentFlags().GetBool("json")
	if err != nil {
		return PackInstallResult{}, err
	}
	if jsonOutput {
		stdout = io.Discard
		stderr = io.Discard
	}
	if err := process(command.Context(), []string{python, "-m", "pip", "install", requirement}, command.InOrStdin(), stdout, stderr); err != nil {
		return PackInstallResult{}, domain.NewError(domain.CodeLocalStorage, fmt.Errorf("install PyPI language packs: %w", err))
	}
	return PackInstallResult{Languages: languages, Requirement: requirement}, nil
}

func normalizePackLanguages(arguments []string) ([]string, error) {
	requested := make(map[string]bool, len(arguments))
	for _, argument := range arguments {
		language := strings.ToLower(strings.TrimSpace(argument))
		if !knownPackLanguage(language) {
			return nil, domain.NewError(domain.CodeValidation, fmt.Errorf("unsupported pack language %q", argument))
		}
		requested[language] = true
	}
	languages := make([]string, 0, len(requested))
	for _, language := range packLanguages {
		if requested[language] {
			languages = append(languages, language)
		}
	}
	return languages, nil
}

func knownPackLanguage(value string) bool {
	for _, language := range packLanguages {
		if language == value {
			return true
		}
	}
	return false
}

func validPackageVersion(value string) bool {
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if part == "" || len(part) > 1 && part[0] == '0' {
			return false
		}
		for _, character := range part {
			if character < '0' || character > '9' {
				return false
			}
		}
	}
	return true
}
