package analysis

import (
	"regexp"
	"strings"

	"github.com/amirhasanzadehpy/Pogo/internal/schema"
)

var importedNamePattern = regexp.MustCompile(`(?m)([A-Za-z_][A-Za-z0-9_]*)\s*(?:as\s+([A-Za-z_][A-Za-z0-9_]*))?\s*(?:,|$)`)

func ResolveDefinitionSyntax(source []byte, offset int, graph *schema.Graph, syntax []SyntaxStatement) (schema.SourceRange, bool) {
	if graph == nil || offset < 0 || offset > len(source) {
		return schema.SourceRange{}, false
	}
	if context, ok := AnalyzeSyntax(source, offset, graph, syntax); ok && context.Identifier != "" {
		if sourceRange, resolved := contextSourceRange(context, graph); resolved {
			return sourceRange, true
		}
	}
	label, ok := resolveModelReference(source, offset, graph, syntax)
	if !ok {
		return schema.SourceRange{}, false
	}
	return graph.ModelSourceRange(label)
}

func contextSourceRange(context Context, graph *schema.Graph) (schema.SourceRange, bool) {
	switch context.Kind {
	case ContextORMPath:
		if context.Path == nil || context.Path.OnSeparator {
			return schema.SourceRange{}, false
		}
		resolved, ok := ResolvePathSegment(graph, context.Value.CanonicalLabel, context.Path.Mode, context.Path.Segments, context.Path.ActiveSegment)
		if !ok || resolved.Field == nil || resolved.Text == "" {
			return schema.SourceRange{}, false
		}
		return resolved.Field.SourceRange()
	case ContextMethodMember:
		if context.Method == nil {
			return schema.SourceRange{}, false
		}
		return context.Method.SourceRange()
	case ContextQueryKeyword:
		access, ok := graph.QueryAccess(context.Value.CanonicalLabel, context.Identifier)
		if !ok || access.Field == nil {
			return schema.SourceRange{}, false
		}
		return access.Field.SourceRange()
	case ContextInstanceMember:
		access, ok := graph.InstanceAccess(context.Value.CanonicalLabel, context.Identifier)
		if !ok || access.Field == nil {
			return schema.SourceRange{}, false
		}
		return access.Field.SourceRange()
	case ContextModelMember:
		if manager, ok := graph.Manager(context.Value.CanonicalLabel, context.Identifier); ok {
			return manager.SourceRange()
		}
		access, ok := graph.InstanceAccess(context.Value.CanonicalLabel, context.Identifier)
		if !ok || access.Field == nil {
			return schema.SourceRange{}, false
		}
		return access.Field.SourceRange()
	default:
		return schema.SourceRange{}, false
	}
}

func resolveModelReference(source []byte, offset int, graph *schema.Graph, syntax []SyntaxStatement) (string, bool) {
	reference := identifierRange(source, offset)
	if reference.Start == reference.End || !referenceIsCode(source, reference) {
		return "", false
	}

	imports, _ := buildEnvironment(source[:offset], graph, syntax, offset)
	start := reference.Start
	for start > 0 && source[start-1] == '.' {
		component := start - 1
		for component > 0 && isIdentifierByte(source[component-1]) {
			component--
		}
		if component == start-1 {
			break
		}
		start = component
	}
	if label, ok := resolveClass(string(source[start:reference.End]), imports, graph); ok {
		return label, true
	}

	for _, statement := range syntax {
		if statement.Guarded || reference.Start < statement.Start || reference.End > statement.End || statement.End > len(source) {
			continue
		}
		text := string(source[statement.Start:statement.End])
		match := fromImportPattern.FindStringSubmatch(text)
		if match == nil {
			continue
		}
		importAt := strings.Index(text, "import")
		if importAt < 0 || reference.Start-statement.Start <= importAt+len("import") {
			continue
		}
		identifier := string(source[reference.Start:reference.End])
		for _, item := range importedNamePattern.FindAllStringSubmatch(strings.Trim(strings.TrimSpace(match[2]), "()"), -1) {
			if item[1] == identifier || item[2] == identifier {
				if label, ok := graph.CanonicalLabelForClass(match[1] + "." + item[1]); ok {
					return label, true
				}
			}
		}
	}
	return "", false
}

func referenceIsCode(source []byte, reference ByteRange) bool {
	mask := pythonCodeMask(source, len(source))
	for index := reference.Start; index < reference.End; index++ {
		if index >= len(mask) || !mask[index] {
			return false
		}
	}
	return true
}
