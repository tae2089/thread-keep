package indexing

import (
	"context"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
)

type Language string

const (
	Go         Language = "go"
	TypeScript Language = "typescript"
	JavaScript Language = "javascript"
	Python     Language = "python"
	Java       Language = "java"
	Kotlin     Language = "kotlin"
	Rust       Language = "rust"
)

var externalPackLanguages = [...]Language{TypeScript, JavaScript, Python, Java, Kotlin, Rust}

type Candidate struct {
	Language Language
	Files    []string
}

func Detect(root string) ([]Candidate, error) {
	return DetectContext(context.Background(), root)
}

func DetectContext(ctx context.Context, root string) ([]Candidate, error) {
	files := map[Language][]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "vendor", "node_modules":
				if path != root {
					return filepath.SkipDir
				}
			}
			return nil
		}
		language, ok := languageForPath(entry.Name())
		if !ok {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files[language] = append(files[language], filepath.ToSlash(relative))
		return nil
	})
	if err != nil {
		return nil, err
	}
	var candidates []Candidate
	for _, language := range []Language{Go, TypeScript, JavaScript, Python, Java, Kotlin, Rust} {
		if len(files[language]) == 0 {
			continue
		}
		sort.Strings(files[language])
		candidates = append(candidates, Candidate{Language: language, Files: files[language]})
	}
	return candidates, nil
}

func languageForPath(name string) (Language, bool) {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".go":
		return Go, true
	case ".ts", ".tsx", ".mts", ".cts":
		return TypeScript, true
	case ".js", ".jsx", ".mjs", ".cjs":
		return JavaScript, true
	case ".py", ".pyi", ".pyw":
		return Python, true
	case ".java":
		return Java, true
	case ".kt", ".kts":
		return Kotlin, true
	case ".rs":
		return Rust, true
	default:
		return "", false
	}
}

func isExternalPackLanguage(language Language) bool {
	for _, candidate := range externalPackLanguages {
		if language == candidate {
			return true
		}
	}
	return false
}

func isExternalPackID(identifier string) bool {
	for _, language := range externalPackLanguages {
		if identifier == packID(language) {
			return true
		}
	}
	return false
}
