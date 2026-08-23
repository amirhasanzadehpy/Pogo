package analysis

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/amirhasanzadehpy/Pogo/internal/schema"
)

var importedNamePattern = regexp.MustCompile(`(?m)([A-Za-z_][A-Za-z0-9_]*)\s*(?:as\s+([A-Za-z_][A-Za-z0-9_]*))?\s*(?:,|$)`)

func ResolveDefinitionSyntax(source []byte, offset int, graph *schema.Graph, syntax []SyntaxStatement) (schema.SourceRange, bool) {
	return ResolveDefinitionSyntaxFile(source, offset, graph, syntax, "")
}

func ResolveDefinitionSyntaxFile(source []byte, offset int, graph *schema.Graph, syntax []SyntaxStatement, filePath string) (schema.SourceRange, bool) {
	if graph == nil || offset < 0 || offset > len(source) {
		return schema.SourceRange{}, false
	}
	if context, ok := AnalyzeSyntaxFile(source, offset, graph, syntax, filePath); ok && context.Identifier != "" {
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
	case ContextMetaField:
		access, ok := graph.InstanceAccess(context.Value.CanonicalLabel, context.Identifier)
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

// ResolveAnnotationAliasDefinition locates the .annotate()/.alias() keyword argument that
// introduced the QuerySet annotation referenced at offset, scanning the literal call chain
// that produced the current value. Annotation aliases are a syntactic, same-file construct
// (not part of the schema graph), so this returns a plain in-document range rather than a
// schema.SourceRange.
func ResolveAnnotationAliasDefinition(source []byte, offset int, graph *schema.Graph, syntax []SyntaxStatement, filePath string) (ByteRange, bool) {
	ref, _, ok := resolveAnnotationUsage(source, offset, graph, syntax, filePath)
	if !ok {
		return ByteRange{}, false
	}
	return ref.span, true
}

// AnnotationReturnType infers the Python return type of the QuerySet annotation referenced at
// offset (e.g. "int" for Count(...), "bool" for Exists(...), or a field's Django type when the
// annotation wraps a resolvable model field such as Sum("price") or F("price")). It returns
// false when the alias exists but its expression isn't one of the recognized shapes.
func AnnotationReturnType(source []byte, offset int, graph *schema.Graph, syntax []SyntaxStatement, filePath string) (string, bool) {
	ref, canonicalLabel, ok := resolveAnnotationUsage(source, offset, graph, syntax, filePath)
	if !ok {
		return "", false
	}
	return inferAnnotationReturnType(string(source[ref.value.Start:ref.value.End]), canonicalLabel, graph)
}

// resolveAnnotationUsage locates the .annotate()/.alias() keyword argument backing the
// annotation alias referenced at offset, covering every shape the query-path analyzer surfaces
// an annotation alias through: values()/values_list() and filter()/exclude()/get() path
// segments (including the single-segment ContextQueryKeyword form and lookups nested in Q(...)),
// and attribute access on a resolved model instance (obj.alias).
func resolveAnnotationUsage(source []byte, offset int, graph *schema.Graph, syntax []SyntaxStatement, filePath string) (annotationAliasRef, string, bool) {
	if graph == nil || offset < 0 || offset > len(source) {
		return annotationAliasRef{}, "", false
	}
	context, ok := AnalyzeSyntaxFile(source, offset, graph, syntax, filePath)
	if !ok || context.Identifier == "" || !isAnnotationAlias(context.Value.Annotations, context.Identifier) {
		return annotationAliasRef{}, "", false
	}
	var chain ByteRange
	switch context.Kind {
	case ContextInstanceMember:
		replacement := identifierRange(source, offset)
		if replacement.Start == 0 || source[replacement.Start-1] != '.' {
			return annotationAliasRef{}, "", false
		}
		chain = expressionBefore(source, replacement.Start-1)
	case ContextQueryKeyword:
		imports, _ := buildEnvironmentAtPath(source[:offset], graph, syntax, offset, filePath)
		receiver, ok := queryKeywordReceiver(source, offset, imports)
		if !ok {
			return annotationAliasRef{}, "", false
		}
		chain = receiver
	case ContextORMPath:
		if context.Path == nil || context.Path.OnSeparator || context.Path.ActiveSegment != 0 {
			return annotationAliasRef{}, "", false
		}
		if context.Path.Mode != PathLookup && context.Path.Mode != PathProjection {
			return annotationAliasRef{}, "", false
		}
		receiver, ok := ormPathChainReceiver(source, offset, graph, syntax, filePath)
		if !ok {
			return annotationAliasRef{}, "", false
		}
		chain = receiver
	default:
		return annotationAliasRef{}, "", false
	}
	ref, ok := findChainAnnotationRef(source, chain, context.Identifier)
	if !ok {
		return annotationAliasRef{}, "", false
	}
	return ref, context.Value.CanonicalLabel, true
}

// ormPathChainReceiver finds the queryset chain a ContextORMPath string segment belongs to.
// For a direct string/keyword path argument (e.g. .values("x"), .filter(x=1)) that's simply the
// enclosing call's receiver. For a segment nested inside an expression function's argument
// (e.g. F("x") or Sum("x") inside .annotate(...)), it looks past that expression call to the
// annotate()/alias()/aggregate() call that actually owns the chain.
func ormPathChainReceiver(source []byte, offset int, graph *schema.Graph, syntax []SyntaxStatement, filePath string) (ByteRange, bool) {
	if expression, ok := enclosingFunctionCall(source, offset); ok {
		imports, _ := buildEnvironmentAtPath(source[:offset], graph, syntax, offset, filePath)
		if isAnnotationExpressionFunction(expandImport(expression.name, imports)) {
			call, ok := enclosingCallBefore(source, offset, expression.opening)
			if !ok || call.method != "annotate" && call.method != "alias" && call.method != "aggregate" {
				return ByteRange{}, false
			}
			return call.receiver, true
		}
	}
	call, ok := enclosingCall(source, offset)
	if !ok {
		return ByteRange{}, false
	}
	return call.receiver, true
}

// queryKeywordReceiver finds the receiver of the filter()/exclude()/get() call backing a bare
// (no "__") keyword segment, looking past an enclosing Q(...) wrapper when present.
func queryKeywordReceiver(source []byte, offset int, imports map[string]string) (ByteRange, bool) {
	if expression, ok := enclosingFunctionCall(source, offset); ok && expandImport(expression.name, imports) == "django.db.models.Q" {
		call, ok := enclosingCallBefore(source, offset, expression.opening)
		if !ok {
			return ByteRange{}, false
		}
		return call.receiver, true
	}
	call, ok := enclosingCall(source, offset)
	if !ok {
		return ByteRange{}, false
	}
	return call.receiver, true
}

func findChainAnnotationRef(source []byte, chain ByteRange, name string) (annotationAliasRef, bool) {
	if name == "" || chain.Start < 0 || chain.End > len(source) || chain.Start >= chain.End {
		return annotationAliasRef{}, false
	}
	text := string(source[chain.Start:chain.End])
	code := pythonCodeMask(source[chain.Start:chain.End], len(text))
	depth := 0
	var found annotationAliasRef
	ok := false
	for index := 0; index < len(text); index++ {
		if index < len(code) && !code[index] {
			continue
		}
		switch text[index] {
		case '(', '[', '{':
			depth++
			continue
		case ')', ']', '}':
			if depth > 0 {
				depth--
			}
			continue
		}
		if depth != 0 || text[index] != '.' {
			continue
		}
		nameStart := index + 1
		nameEnd := nameStart
		for nameEnd < len(text) && isIdentifierByte(text[nameEnd]) {
			nameEnd++
		}
		if method := text[nameStart:nameEnd]; method != "annotate" && method != "alias" {
			continue
		}
		callStart := nameEnd
		for callStart < len(text) && (text[callStart] == ' ' || text[callStart] == '\t') {
			callStart++
		}
		if callStart >= len(text) || text[callStart] != '(' {
			continue
		}
		callEnd, matched := skipCall(text, callStart)
		if !matched {
			break
		}
		for _, ref := range annotationAliasRefs(text[callStart+1 : callEnd-1]) {
			if ref.name == name {
				base := chain.Start + callStart + 1
				found = annotationAliasRef{
					name:  ref.name,
					span:  ByteRange{Start: base + ref.span.Start, End: base + ref.span.End},
					value: ByteRange{Start: base + ref.value.Start, End: base + ref.value.End},
				}
				ok = true
			}
		}
		index = callEnd - 1
	}
	return found, ok
}

type annotationAliasRef struct {
	name  string
	span  ByteRange
	value ByteRange
}

func annotationAliasRefs(args string) []annotationAliasRef {
	var refs []annotationAliasRef
	depth := 0
	segStart := 0
	var quote byte
	escaped := false
	for index := 0; index <= len(args); index++ {
		var ch byte
		if index < len(args) {
			ch = args[index]
		}
		if quote != 0 {
			if escaped {
				escaped = false
				continue
			}
			if ch == '\\' {
				escaped = true
				continue
			}
			if ch == quote {
				quote = 0
			}
			continue
		}
		switch ch {
		case '\'', '"':
			quote = ch
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			if depth > 0 {
				depth--
			}
		case ',', 0:
			if depth == 0 {
				if ref, ok := keywordArgRef(args, segStart, index); ok {
					refs = append(refs, ref)
				}
				segStart = index + 1
			}
		}
	}
	return refs
}

func keywordArgRef(args string, start, end int) (annotationAliasRef, bool) {
	trimStart, trimEnd := start, end
	for trimStart < trimEnd && isSpace(args[trimStart]) {
		trimStart++
	}
	for trimEnd > trimStart && isSpace(args[trimEnd-1]) {
		trimEnd--
	}
	segment := args[trimStart:trimEnd]
	for index := 0; index < len(segment); index++ {
		switch segment[index] {
		case '(', '[', '{':
			return annotationAliasRef{}, false
		case '=':
			if index == 0 {
				return annotationAliasRef{}, false
			}
			prev := segment[index-1]
			if prev == '!' || prev == '<' || prev == '>' {
				return annotationAliasRef{}, false
			}
			next := byte(0)
			if index+1 < len(segment) {
				next = segment[index+1]
			}
			if next == '=' {
				return annotationAliasRef{}, false
			}
			name := strings.TrimSpace(segment[:index])
			if !identifierText(name) {
				return annotationAliasRef{}, false
			}
			valueStart := trimStart + index + 1
			for valueStart < trimEnd && isSpace(args[valueStart]) {
				valueStart++
			}
			return annotationAliasRef{
				name:  name,
				span:  ByteRange{Start: trimStart, End: trimStart + len(name)},
				value: ByteRange{Start: valueStart, End: trimEnd},
			}, true
		}
	}
	return annotationAliasRef{}, false
}

// inferAnnotationReturnType maps a single .annotate()/.alias() expression to the Python type its
// values will have when read back from a row. It only recognizes a fixed set of common Django
// aggregate/expression shapes; anything else (combined expressions, custom Func subclasses
// without an explicit output_field, ...) is left unresolved rather than guessed at.
func inferAnnotationReturnType(valueText string, canonicalLabel string, graph *schema.Graph) (string, bool) {
	name, args, ok := topLevelCall(strings.TrimSpace(valueText))
	if !ok {
		return "", false
	}
	for _, ref := range annotationAliasRefs(args) {
		if ref.name == "output_field" {
			if fieldClass, ok := topLevelCallName(strings.TrimSpace(args[ref.value.Start:ref.value.End])); ok {
				return fieldClass, true
			}
		}
	}
	switch baseFunctionName(name) {
	case "Count":
		return "int", true
	case "Exists":
		return "bool", true
	case "Concat":
		return "str", true
	case "Length":
		return "int", true
	case "Sum", "Avg", "Min", "Max", "F":
		fieldPath, ok := firstStringLiteralArg(args)
		if !ok {
			return "", false
		}
		return resolveFieldType(canonicalLabel, fieldPath, graph)
	case "Value":
		literal, ok := firstPositionalArg(args)
		if !ok {
			return "", false
		}
		return literalPythonType(literal)
	default:
		return "", false
	}
}

func topLevelCall(expr string) (name string, args string, ok bool) {
	open := strings.IndexByte(expr, '(')
	if open < 0 || expr == "" || expr[len(expr)-1] != ')' {
		return "", "", false
	}
	name = strings.TrimSpace(expr[:open])
	if name == "" || !isDottedIdentifier(name) {
		return "", "", false
	}
	end, matched := skipCall(expr, open)
	if !matched || end != len(expr) {
		return "", "", false
	}
	return name, expr[open+1 : end-1], true
}

func topLevelCallName(expr string) (string, bool) {
	name, _, ok := topLevelCall(expr)
	if !ok {
		return "", false
	}
	return baseFunctionName(name), true
}

func isDottedIdentifier(name string) bool {
	for _, part := range strings.Split(name, ".") {
		if !identifierText(part) {
			return false
		}
	}
	return true
}

func baseFunctionName(name string) string {
	if index := strings.LastIndexByte(name, '.'); index >= 0 {
		return name[index+1:]
	}
	return name
}

func firstPositionalArg(args string) (string, bool) {
	depth := 0
	var quote byte
	escaped := false
	for index := 0; index < len(args); index++ {
		ch := args[index]
		if quote != 0 {
			if escaped {
				escaped = false
			} else if ch == '\\' {
				escaped = true
			} else if ch == quote {
				quote = 0
			}
			continue
		}
		switch ch {
		case '\'', '"':
			quote = ch
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				return strings.TrimSpace(args[:index]), true
			}
		}
	}
	if strings.TrimSpace(args) == "" {
		return "", false
	}
	return strings.TrimSpace(args), true
}

func firstStringLiteralArg(args string) (string, bool) {
	literal, ok := firstPositionalArg(args)
	if !ok || len(literal) < 2 {
		return "", false
	}
	quote := literal[0]
	if quote != '\'' && quote != '"' || literal[len(literal)-1] != quote {
		return "", false
	}
	return literal[1 : len(literal)-1], true
}

func resolveFieldType(canonicalLabel string, fieldPath string, graph *schema.Graph) (string, bool) {
	if canonicalLabel == "" || fieldPath == "" || graph == nil {
		return "", false
	}
	model := canonicalLabel
	segments := strings.Split(fieldPath, "__")
	for index, segment := range segments {
		access, ok := graph.QueryAccess(model, segment)
		if !ok || access.Field == nil {
			return "", false
		}
		if index == len(segments)-1 {
			return access.Field.Type(), true
		}
		related, ok := access.Field.RelatedModel()
		if !ok {
			return "", false
		}
		model = related
	}
	return "", false
}

func literalPythonType(literal string) (string, bool) {
	if literal == "" {
		return "", false
	}
	switch literal {
	case "True", "False":
		return "bool", true
	case "None":
		return "None", true
	}
	if len(literal) >= 2 {
		quote := literal[0]
		if (quote == '\'' || quote == '"') && literal[len(literal)-1] == quote {
			return "str", true
		}
	}
	if _, err := strconv.Atoi(literal); err == nil {
		return "int", true
	}
	if _, err := strconv.ParseFloat(literal, 64); err == nil {
		return "float", true
	}
	return "", false
}
