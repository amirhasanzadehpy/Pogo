package lsp

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"github.com/amirhasanzadehpy/Pogo/internal/analysis"
	"github.com/amirhasanzadehpy/Pogo/internal/schema"
	"github.com/tliron/glsp"
	protocol "github.com/tliron/glsp/protocol_3_16"
)

var markdownTextReplacer = strings.NewReplacer("\\", "\\\\", "*", "\\*", "_", "\\_", "`", "\\`", "[", "\\[", "]", "\\]")

type Features struct {
	documents    *analysis.Store
	cache        *schema.Cache
	notifyMu     sync.RWMutex
	diagnosticMu sync.Mutex
	notify       glsp.NotifyFunc
	closed       bool
}

func (features *Features) Close() {
	if features != nil {
		features.diagnosticMu.Lock()
		features.notifyMu.Lock()
		features.closed = true
		features.notify = nil
		features.notifyMu.Unlock()
		features.diagnosticMu.Unlock()
		features.documents.CloseAll()
	}
}

func (features *Features) Capabilities() protocol.ServerCapabilities {
	openClose := true
	change := protocol.TextDocumentSyncKindIncremental
	includeText := true
	return protocol.ServerCapabilities{
		TextDocumentSync: protocol.TextDocumentSyncOptions{OpenClose: &openClose, Change: &change, Save: protocol.SaveOptions{IncludeText: &includeText}},
		CompletionProvider: &protocol.CompletionOptions{
			TriggerCharacters: []string{".", "_", "\"", "'"},
		},
		HoverProvider:      true,
		DefinitionProvider: true,
		SignatureHelpProvider: &protocol.SignatureHelpOptions{
			TriggerCharacters:   []string{"(", ","},
			RetriggerCharacters: []string{","},
		},
	}
}

func NewFeatures(cache *schema.Cache) (*Features, error) {
	if cache == nil {
		return nil, errors.New("schema cache is required")
	}
	documents, err := analysis.NewStore()
	if err != nil {
		return nil, err
	}
	return &Features{documents: documents, cache: cache}, nil
}

func (features *Features) didOpen(ctx *glsp.Context, params *protocol.DidOpenTextDocumentParams) error {
	if features == nil || params == nil {
		return nil
	}
	features.setNotifier(ctx)
	uri := string(params.TextDocument.URI)
	filePath, validPath := localFilePath(uri)
	if validPath {
		if resolved, err := filepath.EvalSymlinks(filePath); err == nil {
			filePath = resolved
		}
	} else {
		filePath = ""
	}
	if err := features.documents.OpenFile(uri, filePath, int32(params.TextDocument.Version), params.TextDocument.Text); err != nil {
		return err
	}
	features.publishURI(string(params.TextDocument.URI))
	return nil
}

func (features *Features) didChange(ctx *glsp.Context, params *protocol.DidChangeTextDocumentParams) error {
	if features == nil || params == nil {
		return nil
	}
	changes := make([]analysis.Change, 0, len(params.ContentChanges))
	for _, raw := range params.ContentChanges {
		switch change := raw.(type) {
		case protocol.TextDocumentContentChangeEvent:
			if change.Range == nil {
				return errors.New("incremental content change is missing its range")
			}
			changes = append(changes, analysis.Change{
				Range: &analysis.Range{
					Start: analysisPosition(change.Range.Start),
					End:   analysisPosition(change.Range.End),
				},
				Text: change.Text,
			})
		case protocol.TextDocumentContentChangeEventWhole:
			changes = append(changes, analysis.Change{Text: change.Text})
		default:
			return fmt.Errorf("unsupported content change type %T", raw)
		}
	}
	features.setNotifier(ctx)
	if err := features.documents.Change(string(params.TextDocument.URI), int32(params.TextDocument.Version), changes); err != nil {
		return err
	}
	features.publishURI(string(params.TextDocument.URI))
	return nil
}

func (features *Features) didClose(ctx *glsp.Context, params *protocol.DidCloseTextDocumentParams) error {
	if features != nil && params != nil {
		features.setNotifier(ctx)
		features.documents.Close(string(params.TextDocument.URI))
		features.clearDiagnostics(string(params.TextDocument.URI))
	}
	return nil
}

func (features *Features) completion(_ *glsp.Context, params *protocol.CompletionParams) (any, error) {
	if features == nil || params == nil {
		return nil, nil
	}
	return features.Completion(string(params.TextDocument.URI), analysisPosition(params.Position))
}

func (features *Features) hover(_ *glsp.Context, params *protocol.HoverParams) (*protocol.Hover, error) {
	if features == nil || params == nil {
		return nil, nil
	}
	return features.Hover(string(params.TextDocument.URI), analysisPosition(params.Position))
}

func (features *Features) signatureHelp(_ *glsp.Context, params *protocol.SignatureHelpParams) (*protocol.SignatureHelp, error) {
	if features == nil || params == nil {
		return nil, nil
	}
	return features.SignatureHelp(string(params.TextDocument.URI), analysisPosition(params.Position))
}

func (features *Features) Completion(uri string, position analysis.Position) (*protocol.CompletionList, error) {
	snapshot, ok := features.documents.Snapshot(uri)
	if !ok {
		return nil, nil
	}
	offset, ok := analysis.ByteOffset(snapshot.Source, position)
	if !ok {
		return nil, errors.New("completion position is not valid for the document")
	}
	graph, _ := features.cache.Load()
	filePath := snapshot.FilePath
	if filePath == "" {
		filePath, _ = localFilePath(uri)
	}
	context, ok := analysis.AnalyzeSyntaxFile(snapshot.Source, offset, graph, snapshot.Syntax, filePath)
	if !ok {
		return nil, nil
	}
	replacement, ok := protocolRange(snapshot.Source, context.Replacement)
	if !ok {
		return nil, errors.New("completion replacement range is invalid")
	}
	items := make([]protocol.CompletionItem, 0, 16)
	switch context.Kind {
	case analysis.ContextQueryKeyword:
		graph.VisitQueryFields(context.Value.CanonicalLabel, func(access schema.FieldAccess) bool {
			if strings.HasPrefix(access.Name, context.Identifier) {
				items = append(items, fieldCompletion(access, replacement))
			}
			return true
		})
	case analysis.ContextInstanceMember:
		graph.VisitInstanceFields(context.Value.CanonicalLabel, func(access schema.FieldAccess) bool {
			if strings.HasPrefix(access.Name, context.Identifier) {
				items = append(items, fieldCompletion(access, replacement))
			}
			return true
		})
	case analysis.ContextModelMember:
		graph.VisitInstanceFields(context.Value.CanonicalLabel, func(access schema.FieldAccess) bool {
			if strings.HasPrefix(access.Name, context.Identifier) {
				items = append(items, fieldCompletion(access, replacement))
			}
			return true
		})
		graph.VisitManagers(context.Value.CanonicalLabel, func(manager *schema.ManagerRef) bool {
			if strings.HasPrefix(manager.Name(), context.Identifier) {
				items = append(items, managerCompletion(manager, replacement))
			}
			return true
		})
	case analysis.ContextORMPath:
		if context.Path.OnSeparator {
			return nil, nil
		}
		for _, candidate := range analysis.CompletePath(graph, context.Value.CanonicalLabel, context.Path.Mode, context.Path.Segments, context.Path.ActiveSegment) {
			items = append(items, pathCompletion(candidate, replacement))
		}
	case analysis.ContextMethodMember:
		analysis.VisitMethods(graph, context.Value, func(method *schema.MethodRef) bool {
			if strings.HasPrefix(method.Name(), context.Identifier) {
				items = append(items, methodCompletion(method, replacement))
			}
			return true
		})
	}
	if len(items) == 0 {
		return nil, nil
	}
	return &protocol.CompletionList{IsIncomplete: false, Items: items}, nil
}

func (features *Features) Hover(uri string, position analysis.Position) (*protocol.Hover, error) {
	snapshot, ok := features.documents.Snapshot(uri)
	if !ok {
		return nil, nil
	}
	offset, ok := analysis.ByteOffset(snapshot.Source, position)
	if !ok {
		return nil, errors.New("hover position is not valid for the document")
	}
	graph, _ := features.cache.Load()
	filePath := snapshot.FilePath
	if filePath == "" {
		filePath, _ = localFilePath(uri)
	}
	context, ok := analysis.AnalyzeSyntaxFile(snapshot.Source, offset, graph, snapshot.Syntax, filePath)
	if !ok || context.Identifier == "" {
		return nil, nil
	}
	var field schema.FieldAccess
	switch context.Kind {
	case analysis.ContextORMPath:
		if context.Path.OnSeparator {
			return nil, nil
		}
		resolved, exists := analysis.ResolvePathSegment(graph, context.Value.CanonicalLabel, context.Path.Mode, context.Path.Segments, context.Path.ActiveSegment)
		if !exists || resolved.Field == nil || resolved.Text == "" {
			return nil, nil
		}
		range_, valid := protocolRange(snapshot.Source, resolved.Range)
		if !valid {
			return nil, nil
		}
		content := fieldMarkdown(schema.FieldAccess{Name: resolved.Text, Field: resolved.Field})
		switch resolved.Kind {
		case analysis.PathCandidateTransform:
			content += fmt.Sprintf("\n\nDjango transform: `%s`", markdownCode(resolved.Text))
		case analysis.PathCandidateLookup:
			content += fmt.Sprintf("\n\nDjango lookup: `%s`", markdownCode(resolved.Text))
		}
		return &protocol.Hover{Contents: protocol.MarkupContent{Kind: protocol.MarkupKindMarkdown, Value: content}, Range: &range_}, nil
	case analysis.ContextMethodMember:
		if context.Method == nil {
			return nil, nil
		}
		range_, valid := protocolRange(snapshot.Source, context.Replacement)
		if !valid {
			return nil, nil
		}
		return &protocol.Hover{Contents: protocol.MarkupContent{Kind: protocol.MarkupKindMarkdown, Value: methodMarkdown(context.Method)}, Range: &range_}, nil
	case analysis.ContextQueryKeyword:
		field, ok = graph.QueryAccess(context.Value.CanonicalLabel, context.Identifier)
	case analysis.ContextInstanceMember:
		field, ok = graph.InstanceAccess(context.Value.CanonicalLabel, context.Identifier)
	case analysis.ContextModelMember:
		manager, exists := graph.Manager(context.Value.CanonicalLabel, context.Identifier)
		if !exists {
			field, ok = graph.InstanceAccess(context.Value.CanonicalLabel, context.Identifier)
			break
		}
		range_, valid := protocolRange(snapshot.Source, context.Replacement)
		if !valid {
			return nil, nil
		}
		return &protocol.Hover{
			Contents: protocol.MarkupContent{Kind: protocol.MarkupKindMarkdown, Value: managerMarkdown(manager)},
			Range:    &range_,
		}, nil
	}
	if !ok || field.Field == nil {
		return nil, nil
	}
	range_, valid := protocolRange(snapshot.Source, context.Replacement)
	if !valid {
		return nil, nil
	}
	return &protocol.Hover{
		Contents: protocol.MarkupContent{Kind: protocol.MarkupKindMarkdown, Value: fieldMarkdown(field)},
		Range:    &range_,
	}, nil
}

func (features *Features) SignatureHelp(uri string, position analysis.Position) (*protocol.SignatureHelp, error) {
	snapshot, ok := features.documents.Snapshot(uri)
	if !ok {
		return nil, nil
	}
	offset, ok := analysis.ByteOffset(snapshot.Source, position)
	if !ok {
		return nil, errors.New("signature help position is not valid for the document")
	}
	graph, _ := features.cache.Load()
	context, ok := analysis.AnalyzeSignatureSyntax(snapshot.Source, offset, graph, snapshot.Syntax)
	if !ok {
		return nil, nil
	}
	signature, ok := context.Method.Signature()
	if !ok {
		return nil, nil
	}
	parsed, ok := parseSignature(signature)
	if !ok {
		return nil, nil
	}
	parameters := make([]protocol.ParameterInformation, len(parsed.parameters))
	for index, parameter := range parsed.parameters {
		parameters[index] = protocol.ParameterInformation{Label: parameter.label}
	}
	information := protocol.SignatureInformation{Label: context.Method.Name() + signature, Parameters: parameters}
	if docstring, exists := context.Method.Docstring(); exists && strings.TrimSpace(docstring) != "" {
		information.Documentation = protocol.MarkupContent{Kind: protocol.MarkupKindMarkdown, Value: markdownText(strings.TrimSpace(docstring))}
	}
	help := &protocol.SignatureHelp{Signatures: []protocol.SignatureInformation{information}}
	if active, exists := parsed.activeParameter(context.PositionalIndex, context.Keyword); exists {
		value := protocol.UInteger(active)
		help.ActiveParameter = &value
	}
	return help, nil
}

func fieldCompletion(access schema.FieldAccess, replacement protocol.Range) protocol.CompletionItem {
	kind := protocol.CompletionItemKindField
	detail := access.Field.Type()
	sortText := fmt.Sprintf("%d-%s", access.Kind, access.Name)
	documentation := protocol.MarkupContent{Kind: protocol.MarkupKindMarkdown, Value: fieldMarkdown(access)}
	return protocol.CompletionItem{
		Label:         access.Name,
		Kind:          &kind,
		Detail:        &detail,
		Documentation: documentation,
		SortText:      &sortText,
		TextEdit:      protocol.TextEdit{Range: replacement, NewText: access.Name},
	}
}

func managerCompletion(manager *schema.ManagerRef, replacement protocol.Range) protocol.CompletionItem {
	kind := protocol.CompletionItemKindProperty
	detail := manager.OwnerClass()
	sortText := "0-" + manager.Name()
	return protocol.CompletionItem{
		Label:         manager.Name(),
		Kind:          &kind,
		Detail:        &detail,
		Documentation: protocol.MarkupContent{Kind: protocol.MarkupKindMarkdown, Value: managerMarkdown(manager)},
		SortText:      &sortText,
		TextEdit:      protocol.TextEdit{Range: replacement, NewText: manager.Name()},
	}
}

func pathCompletion(candidate analysis.PathCandidate, replacement protocol.Range) protocol.CompletionItem {
	if candidate.Kind == analysis.PathCandidateField {
		item := fieldCompletion(schema.FieldAccess{Name: candidate.Name, Field: candidate.Field}, replacement)
		sortText := fmt.Sprintf("%d-%s", candidate.Rank, candidate.Name)
		item.SortText = &sortText
		return item
	}
	kind := protocol.CompletionItemKindKeyword
	detail := "Django transform"
	if candidate.Kind == analysis.PathCandidateLookup {
		detail = "Django lookup"
	}
	sortText := fmt.Sprintf("%d-%s", candidate.Rank, candidate.Name)
	documentation := fmt.Sprintf("%s on `%s`", detail, markdownCode(candidate.Field.Type()))
	return protocol.CompletionItem{
		Label: candidate.Name, Kind: &kind, Detail: &detail, SortText: &sortText,
		Documentation: protocol.MarkupContent{Kind: protocol.MarkupKindMarkdown, Value: documentation},
		TextEdit:      protocol.TextEdit{Range: replacement, NewText: candidate.Name},
	}
}

func methodCompletion(method *schema.MethodRef, replacement protocol.Range) protocol.CompletionItem {
	kind := protocol.CompletionItemKindMethod
	signature, _ := method.Signature()
	detail := method.OwnerClass() + "." + method.Name() + signature
	sortText := "0-" + method.Name()
	return protocol.CompletionItem{
		Label: method.Name(), Kind: &kind, Detail: &detail, SortText: &sortText,
		Documentation: protocol.MarkupContent{Kind: protocol.MarkupKindMarkdown, Value: methodMarkdown(method)},
		TextEdit:      protocol.TextEdit{Range: replacement, NewText: method.Name()},
	}
}

func fieldMarkdown(access schema.FieldAccess) string {
	field := access.Field
	var content strings.Builder
	fmt.Fprintf(&content, "**%s**  \n`%s`", markdownText(access.Name), markdownCode(field.Type()))
	if dbType, ok := field.DBType(); ok && dbType != "" {
		fmt.Fprintf(&content, "\n\n- Database type: `%s`", markdownCode(dbType))
	}
	if column, ok := field.DBColumn(); ok && column != "" {
		fmt.Fprintf(&content, "\n- Database column: `%s`", markdownCode(column))
	}
	fmt.Fprintf(&content, "\n- Null: `%t`", field.IsNullable())
	fmt.Fprintf(&content, "\n- Indexed: `%t`", field.IsDBIndexed())
	fmt.Fprintf(&content, "\n- Unique: `%t`", field.IsUnique())
	fmt.Fprintf(&content, "\n- Primary key: `%t`", field.IsPrimaryKey())
	if related, ok := field.RelatedModel(); ok {
		fmt.Fprintf(&content, "\n- Related model: `%s`", markdownCode(related))
	}
	if source := field.SourceModel(); source != "" {
		fmt.Fprintf(&content, "\n- Source model: `%s`", markdownCode(source))
	}
	if help := strings.TrimSpace(field.HelpText()); help != "" {
		fmt.Fprintf(&content, "\n\n%s", markdownText(help))
	}
	return content.String()
}

func managerMarkdown(manager *schema.ManagerRef) string {
	return fmt.Sprintf("**%s**  \nDjango manager `%s`", markdownText(manager.Name()), markdownCode(manager.OwnerClass()))
}

func methodMarkdown(method *schema.MethodRef) string {
	var content strings.Builder
	signature, _ := method.Signature()
	fmt.Fprintf(&content, "**%s**  \n`%s.%s%s`", markdownText(method.Name()), markdownCode(method.OwnerClass()), markdownCode(method.Name()), markdownCode(signature))
	if docstring, ok := method.Docstring(); ok && strings.TrimSpace(docstring) != "" {
		fmt.Fprintf(&content, "\n\n%s", markdownText(strings.TrimSpace(docstring)))
	}
	if method.AssumedChainable() {
		content.WriteString("\n\n_Chainability inferred from the custom QuerySet contract._")
	}
	return content.String()
}

func protocolRange(source []byte, byteRange analysis.ByteRange) (protocol.Range, bool) {
	start, ok := analysis.PositionAt(source, byteRange.Start)
	if !ok {
		return protocol.Range{}, false
	}
	end, ok := analysis.PositionAt(source, byteRange.End)
	if !ok {
		return protocol.Range{}, false
	}
	return protocol.Range{Start: protocolPosition(start), End: protocolPosition(end)}, true
}

func analysisPosition(position protocol.Position) analysis.Position {
	return analysis.Position{Line: uint32(position.Line), Character: uint32(position.Character)}
}

func protocolPosition(position analysis.Position) protocol.Position {
	return protocol.Position{Line: protocol.UInteger(position.Line), Character: protocol.UInteger(position.Character)}
}

func markdownText(value string) string {
	return markdownTextReplacer.Replace(value)
}

func markdownCode(value string) string {
	return strings.ReplaceAll(value, "`", "\\`")
}
