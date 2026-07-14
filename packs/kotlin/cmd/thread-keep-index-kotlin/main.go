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
	"unicode"

	tree_sitter_kotlin "github.com/tree-sitter-grammars/tree-sitter-kotlin/bindings/go"
	tree_sitter "github.com/tree-sitter/go-tree-sitter"
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
		return errors.New("usage: thread-keep-index-kotlin index --protocol-version=1")
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
	if inputRequest.ProtocolVersion != protocolVersion || inputRequest.Language != "kotlin" || !filepath.IsAbs(inputRequest.RepositoryRoot) || inputRequest.SourceSHA == "" {
		return errors.New("unsupported index request")
	}
	entities, diagnostics, err := index(ctx, inputRequest)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(response{ProtocolVersion: protocolVersion, Indexer: identity{ID: "thread-keep-index-kotlin", Version: "1"}, Language: "kotlin", Entities: entities, Diagnostics: diagnostics})
}

func index(ctx context.Context, input request) ([]entity, []string, error) {
	parser := tree_sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(tree_sitter.NewLanguage(tree_sitter_kotlin.Language())); err != nil {
		return nil, nil, fmt.Errorf("set Kotlin parser language: %w", err)
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
	switch node.Kind() {
	case "class_declaration":
		name := fieldText(node, "name", source)
		if name != "" {
			kind := kotlinClassKind(node, source)
			qualifiedName := qualify(owner.qualifiedName, name)
			entities = append(entities, newEntity(node, source, path, kind, name, qualifiedName))
			nextOwner = scope{qualifiedName: qualifiedName, kind: kind}
		}
	case "object_declaration":
		name := fieldText(node, "name", source)
		if name != "" {
			qualifiedName := qualify(owner.qualifiedName, name)
			entities = append(entities, newEntity(node, source, path, "class", name, qualifiedName))
			nextOwner = scope{qualifiedName: qualifiedName, kind: "class"}
		}
	case "companion_object":
		if owner.qualifiedName != "" && isDeclarationScope(owner.kind) {
			name := fieldText(node, "name", source)
			if name == "" {
				name = "Companion"
			}
			qualifiedName := qualify(owner.qualifiedName, name)
			entities = append(entities, newEntity(node, source, path, "class", name, qualifiedName))
			nextOwner = scope{qualifiedName: qualifiedName, kind: "class"}
		}
	case "type_alias":
		name := fieldText(node, "type", source)
		if name != "" {
			qualifiedName := qualify(owner.qualifiedName, name)
			entities = append(entities, newEntity(node, source, path, "type", name, qualifiedName))
		}
	case "function_declaration":
		name := fieldText(node, "name", source)
		if name != "" {
			kind := "function"
			if isDeclarationScope(owner.kind) {
				kind = "method"
			}
			qualifiedName := qualify(owner.qualifiedName, name)
			entities = append(entities, newEntity(node, source, path, kind, name, qualifiedName))
			nextOwner = scope{qualifiedName: qualifiedName, kind: kind}
		}
	case "primary_constructor", "secondary_constructor":
		if owner.qualifiedName != "" && isDeclarationScope(owner.kind) {
			name := lastSegment(owner.qualifiedName)
			qualifiedName := qualify(owner.qualifiedName, "<init>")
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

func kotlinClassKind(node *tree_sitter.Node, source []byte) string {
	name := node.ChildByFieldName("name")
	if name == nil {
		return "class"
	}
	start := int(node.StartByte())
	end := int(name.StartByte())
	if start < 0 || end < start || end > len(source) {
		return "class"
	}
	prefix := string(source[start:end])
	if hasKeyword(prefix, "enum") {
		return "enum"
	}
	if hasKeyword(prefix, "interface") {
		return "interface"
	}
	return "class"
}

func hasKeyword(value, keyword string) bool {
	for _, token := range strings.FieldsFunc(value, func(r rune) bool {
		return r != '_' && !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if token == keyword {
			return true
		}
	}
	return false
}

func isAnonymousClassBody(parent, child string) bool {
	return child == "class_body" && (parent == "object_literal" || parent == "enum_entry")
}

func isDeclarationScope(value string) bool {
	switch value {
	case "class", "interface", "enum":
		return true
	default:
		return false
	}
}

func lastSegment(value string) string {
	if index := strings.LastIndex(value, "."); index >= 0 {
		return value[index+1:]
	}
	return value
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
