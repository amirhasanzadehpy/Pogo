package lsp

import (
	"errors"
	"fmt"
	"strings"

	"github.com/amirhasanzadehpy/Pogo/internal/analysis"
	"github.com/amirhasanzadehpy/Pogo/internal/schema"
	"github.com/tliron/glsp"
	protocol "github.com/tliron/glsp/protocol_3_16"
)

type Features struct {
	documents *analysis.Store
	cache     *schema.Cache
}

func (features *Features) Close() {
	if features != nil {
		features.documents.CloseAll()
	}
}

func (features *Features) Capabilities() protocol.ServerCapabilities {
	openClose := true
	change := protocol.TextDocumentSyncKindIncremental
	return protocol.ServerCapabilities{
		TextDocumentSync: protocol.TextDocumentSyncOptions{OpenClose: &openClose, Change: &change},
		CompletionProvider: &protocol.CompletionOptions{
			TriggerCharacters: []string{"."},
		},
		HoverProvider: true,
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

func (features *Features) didOpen(_ *glsp.Context, params *protocol.DidOpenTextDocumentParams) error {
	if features == nil || params == nil {
		return nil
	}
	return features.documents.Open(string(params.TextDocument.URI), int32(params.TextDocument.Version), params.TextDocument.Text)
}

func (features *Features) didChange(_ *glsp.Context, params *protocol.DidChangeTextDocumentParams) error {
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
	return features.documents.Change(string(params.TextDocument.URI), int32(params.TextDocument.Version), changes)
}

func (features *Features) didClose(_ *glsp.Context, params *protocol.DidCloseTextDocumentParams) error {
	if features != nil && params != nil {
		features.documents.Close(string(params.TextDocument.URI))
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
	context, ok := analysis.AnalyzeSyntax(snapshot.Source, offset, graph, snapshot.Syntax)
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
	context, ok := analysis.AnalyzeSyntax(snapshot.Source, offset, graph, snapshot.Syntax)
	if !ok || context.Identifier == "" {
		return nil, nil
	}
	var field schema.FieldAccess
	switch context.Kind {
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
	replacer := strings.NewReplacer("\\", "\\\\", "*", "\\*", "_", "\\_", "`", "\\`", "[", "\\[", "]", "\\]")
	return replacer.Replace(value)
}

func markdownCode(value string) string {
	return strings.ReplaceAll(value, "`", "\\`")
}
