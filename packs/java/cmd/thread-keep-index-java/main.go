package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	pathpkg "path"
	"path/filepath"
	"sort"
	"strings"

	tree_sitter "github.com/tree-sitter/go-tree-sitter"
	tree_sitter_java "github.com/tree-sitter/tree-sitter-java/bindings/go"
)

const protocolVersion = 1

const maxRequestBytes = 2 << 20

type request struct {
	ProtocolVersion int      `json:"protocol_version"`
	RepositoryRoot  string   `json:"repository_root"`
	SourceSHA       string   `json:"source_sha"`
	Language        string   `json:"language"`
	Files           []string `json:"files"`
}

type response struct {
	ProtocolVersion int      `json:"protocol_version"`
	Indexer         identity `json:"indexer"`
	Language        string   `json:"language"`
	Entities        []entity `json:"entities"`
	Diagnostics     []string `json:"diagnostics"`
}

type identity struct {
	ID      string `json:"id"`
	Version string `json:"version"`
}

type entity struct {
	Path           string `json:"path"`
	Kind           string `json:"kind"`
	Name           string `json:"name"`
	QualifiedName  string `json:"qualified_name"`
	Signature      string `json:"signature"`
	StartLine      int    `json:"start_line"`
	EndLine        int    `json:"end_line"`
	StructuralHash string `json:"structural_hash"`
}

type scope struct {
	qualifiedName string
	kind          string
}

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, arguments []string, input io.Reader, output io.Writer) error {
	if len(arguments) != 2 || arguments[0] != "index" || arguments[1] != "--protocol-version=1" {
		return errors.New("usage: thread-keep-index-java index --protocol-version=1")
	}
	var inputRequest request
	contents, err := io.ReadAll(io.LimitReader(input, maxRequestBytes+1))
	if err != nil {
		return fmt.Errorf("read request: %w", err)
	}
	if len(contents) > maxRequestBytes {
		return errors.New("index request exceeds 2 MiB")
	}
	decoder := json.NewDecoder(strings.NewReader(string(contents)))
	if err := decoder.Decode(&inputRequest); err != nil {
		return fmt.Errorf("decode request: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("index request contains more than one JSON value")
	}
	if inputRequest.ProtocolVersion != protocolVersion || inputRequest.Language != "java" || !filepath.IsAbs(inputRequest.RepositoryRoot) || inputRequest.SourceSHA == "" {
		return errors.New("unsupported index request")
	}
	entities, diagnostics, err := index(ctx, inputRequest)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(response{ProtocolVersion: protocolVersion, Indexer: identity{ID: "thread-keep-index-java", Version: "1"}, Language: "java", Entities: entities, Diagnostics: diagnostics})
}

func index(ctx context.Context, input request) ([]entity, []string, error) {
	parser := tree_sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(tree_sitter.NewLanguage(tree_sitter_java.Language())); err != nil {
		return nil, nil, fmt.Errorf("set Java parser language: %w", err)
	}
	entities := make([]entity, 0)
	diagnostics := make([]string, 0)
	for _, relative := range input.Files {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		contents, err := readRepositoryFile(input.RepositoryRoot, relative)
		if err != nil {
			return nil, nil, fmt.Errorf("read %s: %w", relative, err)
		}
		tree := parser.Parse(contents, nil)
		if tree == nil {
			return nil, nil, fmt.Errorf("parse %s", relative)
		}
		root := tree.RootNode()
		if root.HasError() {
			tree.Close()
			return nil, nil, fmt.Errorf("syntax error in %s", relative)
		}
		entities = append(entities, collect(root, contents, filepath.ToSlash(relative), scope{})...)
		tree.Close()
	}
	entities = mergeEntities(entities)
	sort.Slice(entities, func(i, j int) bool {
		if entities[i].Path == entities[j].Path {
			return entities[i].QualifiedName < entities[j].QualifiedName
		}
		return entities[i].Path < entities[j].Path
	})
	return entities, diagnostics, nil
}

func readRepositoryFile(root, relative string) ([]byte, error) {
	if !validRelativePath(relative) {
		return nil, fmt.Errorf("unsafe requested path %q", relative)
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("resolve repository root: %w", err)
	}
	path := filepath.Join(root, filepath.FromSlash(relative))
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect %s: %w", relative, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("unsafe requested symlink %q", relative)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", relative, err)
	}
	within, err := filepath.Rel(resolvedRoot, resolved)
	if err != nil || within == ".." || strings.HasPrefix(within, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("requested path escapes repository %q", relative)
	}
	return os.ReadFile(resolved)
}

func validRelativePath(value string) bool {
	return value != "" && !filepath.IsAbs(value) && !strings.Contains(value, "\\") && pathpkg.Clean(value) == value && value != "." && !strings.HasPrefix(value, "../")
}

func collect(node *tree_sitter.Node, source []byte, path string, owner scope) []entity {
	var entities []entity
	nextOwner := owner
	if kind, scopeKind, found := declarationKind(node.Kind()); found {
		name := fieldText(node, "name", source)
		if name != "" {
			qualifiedName := qualify(owner.qualifiedName, name)
			entities = append(entities, newEntity(node, source, path, kind, name, qualifiedName))
			if scopeKind != "" {
				nextOwner = scope{qualifiedName: qualifiedName, kind: scopeKind}
			}
		}
	} else if isConstructor(node.Kind()) {
		name := fieldText(node, "name", source)
		if name != "" && isDeclarationScope(owner.kind) {
			qualifiedName := qualify(owner.qualifiedName, "<init>")
			entities = append(entities, newEntity(node, source, path, "method", name, qualifiedName))
			nextOwner = scope{qualifiedName: qualifiedName, kind: "method"}
		}
	} else if isMemberDeclaration(node.Kind()) {
		name := fieldText(node, "name", source)
		if name != "" && isDeclarationScope(owner.kind) {
			qualifiedName := qualify(owner.qualifiedName, name)
			entities = append(entities, newEntity(node, source, path, "method", name, qualifiedName))
			nextOwner = scope{qualifiedName: qualifiedName, kind: "method"}
		}
	}
	for index := uint(0); index < node.NamedChildCount(); index++ {
		child := node.NamedChild(index)
		if isAnonymousClassBody(node.Kind(), child.Kind()) {
			continue
		}
		entities = append(entities, collect(child, source, path, nextOwner)...)
	}
	return entities
}

func declarationKind(value string) (kind, scopeKind string, found bool) {
	switch value {
	case "class_declaration":
		return "class", "class", true
	case "interface_declaration":
		return "interface", "interface", true
	case "enum_declaration":
		return "enum", "enum", true
	case "record_declaration":
		return "type", "record", true
	case "annotation_type_declaration":
		return "interface", "annotation", true
	default:
		return "", "", false
	}
}

func isConstructor(value string) bool {
	return value == "constructor_declaration" || value == "compact_constructor_declaration"
}

func isMemberDeclaration(value string) bool {
	return value == "method_declaration" || value == "annotation_type_element_declaration"
}

func isAnonymousClassBody(parent, child string) bool {
	return child == "class_body" && (parent == "object_creation_expression" || parent == "enum_constant")
}

func isDeclarationScope(value string) bool {
	switch value {
	case "class", "interface", "enum", "record", "annotation":
		return true
	default:
		return false
	}
}

func qualify(owner, name string) string {
	if owner == "" {
		return name
	}
	return owner + "." + name
}

func mergeEntities(input []entity) []entity {
	merged := make([]entity, 0, len(input))
	positions := make(map[string]int, len(input))
	for _, current := range input {
		key := current.Path + "\x00" + current.Kind + "\x00" + current.QualifiedName
		position, found := positions[key]
		if !found {
			positions[key] = len(merged)
			merged = append(merged, current)
			continue
		}
		existing := &merged[position]
		existing.Signature += "\n" + current.Signature
		if current.StartLine < existing.StartLine {
			existing.StartLine = current.StartLine
		}
		if current.EndLine > existing.EndLine {
			existing.EndLine = current.EndLine
		}
		digest := sha256.Sum256([]byte(existing.Signature))
		existing.StructuralHash = hex.EncodeToString(digest[:])
	}
	return merged
}

func fieldText(node *tree_sitter.Node, field string, source []byte) string {
	child := node.ChildByFieldName(field)
	if child == nil {
		return ""
	}
	return strings.TrimSpace(child.Utf8Text(source))
}

func newEntity(node *tree_sitter.Node, source []byte, path, kind, name, qualifiedName string) entity {
	contents := node.Utf8Text(source)
	digest := sha256.Sum256([]byte(contents))
	return entity{Path: path, Kind: kind, Name: name, QualifiedName: qualifiedName, Signature: strings.TrimSpace(contents), StartLine: int(node.StartPosition().Row) + 1, EndLine: int(node.EndPosition().Row) + 1, StructuralHash: hex.EncodeToString(digest[:])}
}
