package indexer

import (
	"bytes"
	"context"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	pathpkg "path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tae2089/thread-keep/internal/domain"
	"github.com/zeebo/blake3"
)

type Go struct{}

func (Go) Index(ctx context.Context, root, sourceSHA string) ([]domain.Entity, error) {
	var files []string
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
		if strings.EqualFold(filepath.Ext(entry.Name()), ".go") {
			relative, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			files = append(files, filepath.ToSlash(relative))
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk Go files: %w", err)
	}
	return (Go{}).IndexFiles(ctx, root, sourceSHA, files)
}

func (Go) IndexFiles(ctx context.Context, root, sourceSHA string, files []string) ([]domain.Entity, error) {
	files = append([]string(nil), files...)
	sort.Strings(files)
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("resolve repository root: %w", err)
	}

	fset := token.NewFileSet()
	parsed := make([]*ast.File, 0, len(files))
	for _, relative := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		path, err := repositoryFile(root, resolvedRoot, relative)
		if err != nil {
			return nil, err
		}
		file, err := parser.ParseFile(fset, path, nil, parser.ParseComments|parser.AllErrors)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", relative, err)
		}
		parsed = append(parsed, file)
	}

	entities := make([]domain.Entity, 0)
	for i, file := range parsed {
		rel := filepath.FromSlash(files[i])
		prefix := qualifiedPrefix(filepath.ToSlash(filepath.Dir(rel)), file.Name.Name)
		for _, declaration := range file.Decls {
			switch declared := declaration.(type) {
			case *ast.FuncDecl:
				entity, err := functionEntity(fset, rel, prefix, declared, sourceSHA)
				if err != nil {
					return nil, err
				}
				entities = append(entities, entity)
			case *ast.GenDecl:
				if declared.Tok != token.TYPE {
					continue
				}
				for _, specification := range declared.Specs {
					typeSpec, ok := specification.(*ast.TypeSpec)
					if !ok {
						continue
					}
					entity, err := typeEntity(fset, rel, prefix, typeSpec, sourceSHA)
					if err != nil {
						return nil, err
					}
					entities = append(entities, entity)
				}
			}
		}
	}
	sort.Slice(entities, func(i, j int) bool { return entities[i].Key < entities[j].Key })
	return entities, nil
}

func repositoryFile(root, resolvedRoot, relative string) (string, error) {
	if relative == "" || filepath.IsAbs(relative) || strings.Contains(relative, "\\") || pathpkg.Clean(relative) != relative || relative == "." || strings.HasPrefix(relative, "../") || !strings.HasSuffix(strings.ToLower(relative), ".go") {
		return "", fmt.Errorf("unsafe Go source path %q", relative)
	}
	path := filepath.Join(root, filepath.FromSlash(relative))
	info, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("inspect %s: %w", relative, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("unsafe Go source symlink %q", relative)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", relative, err)
	}
	within, err := filepath.Rel(resolvedRoot, resolved)
	if err != nil || within == ".." || strings.HasPrefix(within, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("Go source path escapes repository %q", relative)
	}
	return resolved, nil
}

func qualifiedPrefix(directory, packageName string) string {
	if directory == "." || directory == "" {
		return packageName
	}
	return directory + "/" + packageName
}

func functionEntity(fset *token.FileSet, rel, prefix string, declaration *ast.FuncDecl, sourceSHA string) (domain.Entity, error) {
	signature, err := render(fset, declaration.Type)
	if err != nil {
		return domain.Entity{}, fmt.Errorf("render signature for %s: %w", declaration.Name.Name, err)
	}
	body, err := render(fset, declaration)
	if err != nil {
		return domain.Entity{}, fmt.Errorf("render declaration for %s: %w", declaration.Name.Name, err)
	}
	kind := domain.EntityFunction
	key := prefix + "." + declaration.Name.Name
	if declaration.Recv != nil && len(declaration.Recv.List) > 0 {
		kind = domain.EntityMethod
		receiver, err := render(fset, declaration.Recv.List[0].Type)
		if err != nil {
			return domain.Entity{}, fmt.Errorf("render receiver for %s: %w", declaration.Name.Name, err)
		}
		key = prefix + "." + normalizeReceiver(receiver) + "." + declaration.Name.Name
	}
	return newEntity(fset, declaration.Pos(), declaration.End(), key, kind, declaration.Name.Name, signature, rel, sourceSHA, body), nil
}

func typeEntity(fset *token.FileSet, rel, prefix string, typeSpec *ast.TypeSpec, sourceSHA string) (domain.Entity, error) {
	signature, err := render(fset, typeSpec.Type)
	if err != nil {
		return domain.Entity{}, fmt.Errorf("render type signature for %s: %w", typeSpec.Name.Name, err)
	}
	body, err := render(fset, typeSpec)
	if err != nil {
		return domain.Entity{}, fmt.Errorf("render type declaration for %s: %w", typeSpec.Name.Name, err)
	}
	return newEntity(fset, typeSpec.Pos(), typeSpec.End(), prefix+"."+typeSpec.Name.Name, domain.EntityType, typeSpec.Name.Name, signature, rel, sourceSHA, body), nil
}

func newEntity(fset *token.FileSet, start, end token.Pos, key string, kind domain.EntityKind, name, signature, rel, sourceSHA, body string) domain.Entity {
	position := fset.Position(start)
	endPosition := fset.Position(end)
	hash := blake3.Sum256([]byte(body))
	return domain.Entity{Key: key, Kind: kind, Name: name, Signature: signature, Path: filepath.ToSlash(rel), StartLine: position.Line, EndLine: endPosition.Line, SourceSHA: sourceSHA, StructuralHash: fmt.Sprintf("%x", hash[:])}
}

func render(fset *token.FileSet, node any) (string, error) {
	var buffer bytes.Buffer
	if err := format.Node(&buffer, fset, node); err != nil {
		return "", err
	}
	return buffer.String(), nil
}

func normalizeReceiver(receiver string) string {
	receiver = strings.TrimSpace(receiver)
	receiver = strings.TrimLeft(receiver, "*")
	if bracket := strings.Index(receiver, "["); bracket >= 0 {
		receiver = receiver[:bracket]
	}
	return receiver
}
