package analysis

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	gotreesitter "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"
)

const MaxDocumentSize = 4 * 1024 * 1024

type Position struct {
	Line      uint32
	Character uint32
}

type Range struct {
	Start Position
	End   Position
}

type Change struct {
	Range *Range
	Text  string
}

type Snapshot struct {
	URI     string
	Version int32
	Source  []byte
	Parsed  bool
	Syntax  []SyntaxStatement
	Calls   []SyntaxCall
}

type SyntaxCall struct {
	Range     ByteRange
	Receiver  ByteRange
	Arguments ByteRange
	Method    string
}

type SyntaxStatement struct {
	Start       int
	End         int
	Text        string
	ScopeStart  int
	ScopeEnd    int
	ScopeKind   string
	ScopeMarker bool
	Guarded     bool
}

type document struct {
	version int32
	source  []byte
	tree    *gotreesitter.Tree
	syntax  []SyntaxStatement
	calls   []SyntaxCall
}

type Store struct {
	mu         sync.RWMutex
	documents  map[string]*document
	language   *gotreesitter.Language
	statements *gotreesitter.Query
	scopes     *gotreesitter.Query
	guarded    *gotreesitter.Query
	calls      *gotreesitter.Query
}

func NewStore() (*Store, error) {
	language := grammars.PythonLanguage()
	if language == nil {
		return nil, errors.New("load Python grammar")
	}
	statements, err := gotreesitter.NewQuery(`[
  (import_statement)
  (import_from_statement)
  (assignment)
] @statement`, language)
	if err != nil {
		return nil, fmt.Errorf("compile Python symbol query: %w", err)
	}
	scopes, err := gotreesitter.NewQuery(`[
  (function_definition)
  (class_definition)
] @scope`, language)
	if err != nil {
		return nil, fmt.Errorf("compile Python scope query: %w", err)
	}
	guarded, err := gotreesitter.NewQuery(`[
  (if_statement)
  (for_statement)
  (while_statement)
  (try_statement)
  (with_statement)
  (match_statement)
] @guarded`, language)
	if err != nil {
		return nil, fmt.Errorf("compile Python guarded-block query: %w", err)
	}
	calls, err := gotreesitter.NewQuery(`(call) @call`, language)
	if err != nil {
		return nil, fmt.Errorf("compile Python call query: %w", err)
	}
	return &Store{documents: make(map[string]*document), language: language, statements: statements, scopes: scopes, guarded: guarded, calls: calls}, nil
}

func (store *Store) Open(uri string, version int32, text string) error {
	if uri == "" {
		return errors.New("document URI is required")
	}
	source := []byte(text)
	if err := validateSource(source); err != nil {
		return err
	}
	tree, err := store.parse(source, nil)
	if err != nil {
		return fmt.Errorf("parse opened document: %w", err)
	}
	store.mu.Lock()
	previous := store.documents[uri]
	store.documents[uri] = &document{version: version, source: source, tree: tree, syntax: store.extractSyntax(tree, source), calls: store.extractCalls(tree, source)}
	store.mu.Unlock()
	if previous != nil && previous.tree != nil && previous.tree != tree {
		previous.tree.Release()
	}
	return nil
}

func (store *Store) Change(uri string, version int32, changes []Change) error {
	if len(changes) == 0 {
		return errors.New("document change list is empty")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	current := store.documents[uri]
	if current == nil {
		return errors.New("document is not open")
	}
	if version <= current.version {
		return fmt.Errorf("document version %d is not newer than %d", version, current.version)
	}

	source := append([]byte(nil), current.source...)
	oldTree := current.tree
	incremental := oldTree != nil
	var edits []gotreesitter.InputEdit
	for _, change := range changes {
		if change.Range == nil {
			source = []byte(change.Text)
			incremental = false
			edits = nil
			continue
		}
		start, ok := ByteOffset(source, change.Range.Start)
		if !ok {
			return errors.New("change start is not a valid UTF-16 position")
		}
		end, ok := ByteOffset(source, change.Range.End)
		if !ok || end < start {
			return errors.New("change end is not a valid UTF-16 position")
		}
		replacement := []byte(change.Text)
		if !utf8.Valid(replacement) {
			return errors.New("change text is not valid UTF-8")
		}
		if incremental {
			edits = append(edits, gotreesitter.InputEdit{
				StartByte:   uint32(start),
				OldEndByte:  uint32(end),
				NewEndByte:  uint32(start + len(replacement)),
				StartPoint:  pointForOffset(source, start),
				OldEndPoint: pointForOffset(source, end),
				NewEndPoint: replacementEndPoint(pointForOffset(source, start), replacement),
			})
		}
		updated := make([]byte, 0, len(source)-(end-start)+len(replacement))
		updated = append(updated, source[:start]...)
		updated = append(updated, replacement...)
		updated = append(updated, source[end:]...)
		source = updated
	}
	if err := validateSource(source); err != nil {
		return err
	}
	var incrementalTree *gotreesitter.Tree
	if incremental {
		incrementalTree = oldTree.Copy()
		incremental = incrementalTree != nil
	}
	for _, edit := range edits {
		incrementalTree.Edit(edit)
	}

	var tree *gotreesitter.Tree
	var err error
	if incremental {
		tree, err = store.parse(source, incrementalTree)
	}
	if !incremental || err != nil {
		tree, err = store.parse(source, nil)
	}
	if incrementalTree != nil && incrementalTree != tree {
		incrementalTree.Release()
	}
	if err != nil {
		return fmt.Errorf("parse changed document: %w", err)
	}
	current.version = version
	current.source = source
	current.tree = tree
	current.syntax = store.extractSyntax(tree, source)
	current.calls = store.extractCalls(tree, source)
	if oldTree != nil && oldTree != tree {
		oldTree.Release()
	}
	return nil
}

func (store *Store) Close(uri string) {
	store.mu.Lock()
	current := store.documents[uri]
	delete(store.documents, uri)
	store.mu.Unlock()
	if current != nil && current.tree != nil {
		current.tree.Release()
	}
}

func (store *Store) CloseAll() {
	store.mu.Lock()
	documents := store.documents
	store.documents = make(map[string]*document)
	store.mu.Unlock()
	for _, current := range documents {
		if current.tree != nil {
			current.tree.Release()
		}
	}
}

func (store *Store) Snapshot(uri string) (Snapshot, bool) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	current := store.documents[uri]
	if current == nil {
		return Snapshot{}, false
	}
	return Snapshot{
		URI:     uri,
		Version: current.version,
		Source:  append([]byte(nil), current.source...),
		Parsed:  current.tree != nil,
		Syntax:  append([]SyntaxStatement(nil), current.syntax...),
		Calls:   append([]SyntaxCall(nil), current.calls...),
	}, true
}

func (store *Store) Snapshots() []Snapshot {
	store.mu.RLock()
	snapshots := make([]Snapshot, 0, len(store.documents))
	for uri, current := range store.documents {
		snapshots = append(snapshots, Snapshot{
			URI: uri, Version: current.version, Source: append([]byte(nil), current.source...), Parsed: current.tree != nil,
			Syntax: append([]SyntaxStatement(nil), current.syntax...), Calls: append([]SyntaxCall(nil), current.calls...),
		})
	}
	store.mu.RUnlock()
	sort.Slice(snapshots, func(left, right int) bool { return snapshots[left].URI < snapshots[right].URI })
	return snapshots
}

func (store *Store) Save(uri string, text *string) error {
	if text == nil {
		if _, ok := store.Snapshot(uri); !ok {
			return errors.New("document is not open")
		}
		return nil
	}
	store.mu.RLock()
	current := store.documents[uri]
	if current == nil {
		store.mu.RUnlock()
		return errors.New("document is not open")
	}
	version := current.version
	store.mu.RUnlock()
	return store.saveText(uri, version, *text)
}

func (store *Store) saveText(uri string, version int32, text string) error {
	source := []byte(text)
	if err := validateSource(source); err != nil {
		return err
	}
	tree, err := store.parse(source, nil)
	if err != nil {
		return fmt.Errorf("parse saved document: %w", err)
	}
	store.mu.Lock()
	current := store.documents[uri]
	if current == nil || current.version != version {
		store.mu.Unlock()
		tree.Release()
		return errors.New("saved document was changed concurrently")
	}
	oldTree := current.tree
	current.source = source
	current.tree = tree
	current.syntax = store.extractSyntax(tree, source)
	current.calls = store.extractCalls(tree, source)
	store.mu.Unlock()
	if oldTree != nil && oldTree != tree {
		oldTree.Release()
	}
	return nil
}

func (store *Store) extractCalls(tree *gotreesitter.Tree, source []byte) []SyntaxCall {
	if tree == nil {
		return nil
	}
	var calls []SyntaxCall
	for _, match := range store.calls.Execute(tree) {
		for _, capture := range match.Captures {
			node := capture.Node
			if capture.Name != "call" || node == nil || node.IsError() || node.IsMissing() || node.HasError() {
				continue
			}
			if callHasRecoveredStatement(node, store.language) {
				continue
			}
			function := node.ChildByFieldName("function", store.language)
			arguments := node.ChildByFieldName("arguments", store.language)
			if function == nil || arguments == nil {
				continue
			}
			var receiverRange ByteRange
			methodName := ""
			switch function.Type(store.language) {
			case "attribute":
				receiver := function.ChildByFieldName("object", store.language)
				method := function.ChildByFieldName("attribute", store.language)
				if receiver == nil || method == nil {
					continue
				}
				receiverRange = ByteRange{Start: int(receiver.StartByte()), End: int(receiver.EndByte())}
				methodName = method.Text(source)
			case "identifier":
				methodName = function.Text(source)
			default:
				continue
			}
			calls = append(calls, SyntaxCall{
				Range:     ByteRange{Start: int(node.StartByte()), End: int(node.EndByte())},
				Receiver:  receiverRange,
				Arguments: ByteRange{Start: int(arguments.StartByte()), End: int(arguments.EndByte())},
				Method:    methodName,
			})
		}
	}
	sort.Slice(calls, func(left, right int) bool {
		if calls[left].Range.Start != calls[right].Range.Start {
			return calls[left].Range.Start < calls[right].Range.Start
		}
		return calls[left].Range.End < calls[right].Range.End
	})
	return calls
}

func callHasRecoveredStatement(node *gotreesitter.Node, language *gotreesitter.Language) bool {
	for ancestor := node.Parent(); ancestor != nil; ancestor = ancestor.Parent() {
		if ancestor.IsError() || ancestor.IsMissing() || ancestor.HasError() {
			return true
		}
		typeName := ancestor.Type(language)
		if strings.HasSuffix(typeName, "statement") || typeName == "expression_statement" {
			return false
		}
	}
	return false
}

func (store *Store) extractSyntax(tree *gotreesitter.Tree, source []byte) []SyntaxStatement {
	type syntaxScope struct {
		start int
		end   int
		kind  string
	}
	var scopes []syntaxScope
	for _, match := range store.scopes.Execute(tree) {
		for _, capture := range match.Captures {
			if capture.Name == "scope" && capture.Node != nil {
				scopes = append(scopes, syntaxScope{
					start: int(capture.Node.StartByte()),
					end:   int(capture.Node.EndByte()),
					kind:  capture.Node.Type(store.language),
				})
			}
		}
	}
	type guardedRange struct {
		start int
		end   int
		text  string
	}
	var guardedRanges []guardedRange
	for _, match := range store.guarded.Execute(tree) {
		for _, capture := range match.Captures {
			if capture.Name == "guarded" && capture.Node != nil {
				guardedRanges = append(guardedRanges, guardedRange{start: int(capture.Node.StartByte()), end: int(capture.Node.EndByte()), text: capture.Text(source)})
			}
		}
	}
	scopeFor := func(start, end int) syntaxScope {
		selected := syntaxScope{}
		for _, candidate := range scopes {
			if candidate.start <= start && end <= candidate.end && (selected.end == 0 || candidate.end-candidate.start < selected.end-selected.start) {
				selected = candidate
			}
		}
		return selected
	}
	matches := store.statements.Execute(tree)
	statements := make([]SyntaxStatement, 0, len(matches)+len(scopes))
	for _, scope := range scopes {
		statements = append(statements, SyntaxStatement{
			Start: scope.start, End: scope.end, Text: string(source[scope.start:scope.end]), ScopeStart: scope.start, ScopeEnd: scope.end,
			ScopeKind: scope.kind, ScopeMarker: true,
		})
	}
	for _, guarded := range guardedRanges {
		scope := scopeFor(guarded.start, guarded.end)
		statements = append(statements, SyntaxStatement{
			Start: guarded.start, End: guarded.end, Text: guarded.text,
			ScopeStart: scope.start, ScopeEnd: scope.end, ScopeKind: scope.kind, Guarded: true,
		})
	}
	for _, match := range matches {
		for _, capture := range match.Captures {
			if capture.Name != "statement" || capture.Node == nil {
				continue
			}
			start, end := int(capture.Node.StartByte()), int(capture.Node.EndByte())
			scope := scopeFor(start, end)
			guarded := false
			for _, candidate := range guardedRanges {
				if candidate.start <= start && end <= candidate.end {
					guarded = true
					break
				}
			}
			statements = append(statements, SyntaxStatement{
				Start:      start,
				End:        end,
				Text:       capture.Text(source),
				ScopeStart: scope.start,
				ScopeEnd:   scope.end,
				ScopeKind:  scope.kind,
				Guarded:    guarded,
			})
		}
	}
	for _, scope := range scopes {
		if scope.kind == "function_definition" {
			statements = append(statements, parameterStatements(source, scope.start, scope.end)...)
		}
	}
	return statements
}

func parameterStatements(source []byte, scopeStart, scopeEnd int) []SyntaxStatement {
	open := -1
	for index := scopeStart; index < scopeEnd; index++ {
		if source[index] == '(' {
			open = index
			break
		}
	}
	if open < 0 {
		return nil
	}
	depth := 0
	segmentStart := open + 1
	var statements []SyntaxStatement
	appendSegment := func(end int) {
		text := strings.TrimSpace(string(source[segmentStart:end]))
		if text == "" || text == "/" || text == "*" {
			return
		}
		bindingEnd := len(text)
		if index := strings.IndexAny(text, ":="); index >= 0 {
			bindingEnd = index
		}
		name := strings.TrimLeft(strings.TrimSpace(text[:bindingEnd]), "*")
		if !identifierText(name) {
			return
		}
		if colon := strings.IndexByte(text, ':'); colon >= 0 {
			annotated := name + text[colon:]
			if annotationPattern.MatchString(annotated) {
				text = annotated
			} else {
				text = name + " = __pogo_parameter__"
			}
		} else {
			text = name + " = __pogo_parameter__"
		}
		statements = append(statements, SyntaxStatement{
			Start: segmentStart, End: end, Text: text,
			ScopeStart: scopeStart, ScopeEnd: scopeEnd, ScopeKind: "function_definition",
		})
	}
	for index := open + 1; index < scopeEnd; index++ {
		switch source[index] {
		case '(', '[', '{':
			depth++
		case ')':
			if depth == 0 {
				appendSegment(index)
				return statements
			}
			depth--
		case ']', '}':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				appendSegment(index)
				segmentStart = index + 1
			}
		}
	}
	return statements
}

func (store *Store) parse(source []byte, oldTree *gotreesitter.Tree) (*gotreesitter.Tree, error) {
	parser := gotreesitter.NewParser(store.language)
	if oldTree == nil {
		return parser.Parse(source)
	}
	return parser.ParseIncremental(source, oldTree)
}

func ByteOffset(source []byte, position Position) (int, bool) {
	lineStart, lineEnd, ok := lineBounds(source, position.Line)
	if !ok {
		return 0, false
	}
	units := uint32(0)
	for offset := lineStart; offset < lineEnd; {
		if units == position.Character {
			return offset, true
		}
		r, size := utf8.DecodeRune(source[offset:lineEnd])
		if r == utf8.RuneError && size == 1 {
			return 0, false
		}
		width := uint32(1)
		if r > 0xffff {
			width = 2
		}
		if units+width > position.Character {
			return 0, false
		}
		units += width
		offset += size
	}
	if units == position.Character {
		return lineEnd, true
	}
	return 0, false
}

func PositionAt(source []byte, offset int) (Position, bool) {
	if offset < 0 || offset > len(source) || (offset < len(source) && !utf8.RuneStart(source[offset])) {
		return Position{}, false
	}
	line := uint32(0)
	lineStart := 0
	for index, value := range source[:offset] {
		if value == '\n' {
			line++
			lineStart = index + 1
		}
	}
	character := uint32(0)
	for index := lineStart; index < offset; {
		r, size := utf8.DecodeRune(source[index:offset])
		if r == utf8.RuneError && size == 1 {
			return Position{}, false
		}
		character++
		if r > 0xffff {
			character++
		}
		index += size
	}
	return Position{Line: line, Character: character}, true
}

func lineBounds(source []byte, target uint32) (int, int, bool) {
	line := uint32(0)
	start := 0
	for index, value := range source {
		if value != '\n' {
			continue
		}
		if line == target {
			end := index
			if end > start && source[end-1] == '\r' {
				end--
			}
			return start, end, true
		}
		line++
		start = index + 1
	}
	if line == target {
		return start, len(source), true
	}
	return 0, 0, false
}

func pointForOffset(source []byte, offset int) gotreesitter.Point {
	row := uint32(0)
	column := uint32(0)
	for _, value := range source[:offset] {
		if value == '\n' {
			row++
			column = 0
		} else {
			column++
		}
	}
	return gotreesitter.Point{Row: row, Column: column}
}

func replacementEndPoint(start gotreesitter.Point, replacement []byte) gotreesitter.Point {
	point := start
	for _, value := range replacement {
		if value == '\n' {
			point.Row++
			point.Column = 0
		} else {
			point.Column++
		}
	}
	return point
}

func validateSource(source []byte) error {
	if len(source) > MaxDocumentSize {
		return fmt.Errorf("document exceeds %d-byte limit", MaxDocumentSize)
	}
	if !utf8.Valid(source) {
		return errors.New("document is not valid UTF-8")
	}
	if len(source) > int(^uint32(0)) {
		return errors.New("document exceeds parser byte range")
	}
	return nil
}
