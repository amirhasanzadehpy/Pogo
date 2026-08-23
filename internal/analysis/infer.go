package analysis

import (
	"bytes"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/amirhasanzadehpy/Pogo/internal/schema"
)

type ValueKind uint8

const (
	ValueUnknown ValueKind = iota
	ValueModelClass
	ValueModelInstance
	ValueManager
	ValueQuerySet
)

type Value struct {
	CanonicalLabel string
	Kind           ValueKind
	ManagerName    string
	QuerySetClass  string
	Annotations    []string
}

type ContextKind uint8

const (
	ContextUnknown ContextKind = iota
	ContextQueryKeyword
	ContextModelMember
	ContextInstanceMember
	ContextMethodMember
	ContextORMPath
	ContextMetaField
)

type ByteRange struct {
	Start int
	End   int
}

type Context struct {
	Kind        ContextKind
	Value       Value
	Identifier  string
	Replacement ByteRange
	Method      *schema.MethodRef
	Path        *PathContext
}

var (
	fromImportPattern        = regexp.MustCompile(`(?s)^\s*from\s+([A-Za-z_][A-Za-z0-9_.]*)\s+import\s+(.+)$`)
	importPattern            = regexp.MustCompile(`^\s*import\s+([A-Za-z_][A-Za-z0-9_.]*)(?:\s+as\s+([A-Za-z_][A-Za-z0-9_]*))?\s*$`)
	annotationPattern        = regexp.MustCompile(`(?s)^\s*([A-Za-z_][A-Za-z0-9_]*)\s*:\s*([A-Za-z_][A-Za-z0-9_.]*(?:\[[A-Za-z_][A-Za-z0-9_.]*\])?)(?:\s*=\s*(.+))?$`)
	assignmentPattern        = regexp.MustCompile(`(?s)^\s*([A-Za-z_][A-Za-z0-9_]*)\s*=\s*(.+)$`)
	bindingIdentifierPattern = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)
	definitionBindingPattern = regexp.MustCompile(`(?m)^\s*(?:async\s+def|def|class)\s+([A-Za-z_][A-Za-z0-9_]*)\b`)
	forBindingPattern        = regexp.MustCompile(`(?m)\bfor\s+([^:\n]+?)\s+in\b`)
	asBindingPattern         = regexp.MustCompile(`(?m)\bas\s+(\([^\n)]*\)|[A-Za-z_][A-Za-z0-9_]*)`)
	walrusBindingPattern     = regexp.MustCompile(`\b([A-Za-z_][A-Za-z0-9_]*)\s*:=`)
	adminRegisterPattern     = regexp.MustCompile(`^@([A-Za-z_][A-Za-z0-9_.]*)\s*\(\s*([A-Za-z_][A-Za-z0-9_.]*)\s*\)\s*$`)
	forHeaderPattern         = regexp.MustCompile(`^\s*for\s+([A-Za-z_][A-Za-z0-9_]*)\s+in\s+(.+)\s*:\s*$`)
)

const adminQuerySetBinding = "__pogo_admin_queryset__"

func Analyze(source []byte, offset int, graph *schema.Graph) (Context, bool) {
	return analyze(source, offset, graph, nil, "")
}

func AnalyzeSyntax(source []byte, offset int, graph *schema.Graph, syntax []SyntaxStatement) (Context, bool) {
	return analyze(source, offset, graph, syntax, "")
}

func AnalyzeSyntaxFile(source []byte, offset int, graph *schema.Graph, syntax []SyntaxStatement, filePath string) (Context, bool) {
	return analyze(source, offset, graph, syntax, filePath)
}

func analyze(source []byte, offset int, graph *schema.Graph, syntax []SyntaxStatement, filePath string) (Context, bool) {
	if graph == nil || offset < 0 || offset > len(source) {
		return Context{}, false
	}
	if context, ok := analyzeMetaFieldContext(source, offset, graph, syntax, filePath); ok {
		return context, true
	}
	if context, ok := analyzePathContext(source, offset, graph, syntax, filePath); ok {
		return context, true
	}
	replacement := identifierRange(source, offset)
	identifier := string(source[replacement.Start:replacement.End])
	if strings.Contains(identifier, "__") {
		return Context{}, false
	}
	if replacement.Start == 0 || source[replacement.Start-1] != '.' {
		return Context{}, false
	}
	receiver := expressionBefore(source, replacement.Start-1)
	if receiver.Start == receiver.End {
		return Context{}, false
	}
	value := inferExpressionAtPath(strings.TrimSpace(string(source[receiver.Start:receiver.End])), source[:offset], graph, syntax, offset, filePath)
	switch value.Kind {
	case ValueModelClass:
		return Context{Kind: ContextModelMember, Value: value, Identifier: identifier, Replacement: replacement}, true
	case ValueModelInstance:
		return Context{Kind: ContextInstanceMember, Value: value, Identifier: identifier, Replacement: replacement}, true
	case ValueManager, ValueQuerySet:
		method, exists := methodForValue(graph, value, identifier)
		return Context{Kind: ContextMethodMember, Value: value, Identifier: identifier, Replacement: replacement, Method: methodIf(exists, method)}, true
	default:
		return Context{}, false
	}
}

func analyzeMetaFieldContext(source []byte, offset int, graph *schema.Graph, syntax []SyntaxStatement, filePath string) (Context, bool) {
	label, ok := enclosingMetaModel(syntax, offset, filePath, graph)
	if !ok {
		return Context{}, false
	}
	replacement, ok := metaFieldStringRange(source, offset)
	if !ok || !metaFieldOption(source, replacement.Start) {
		return Context{}, false
	}
	if replacement.Start < replacement.End && (source[replacement.Start] == '-' || source[replacement.Start] == '+') {
		replacement.Start++
	}
	return Context{Kind: ContextMetaField, Value: Value{CanonicalLabel: label, Kind: ValueModelClass}, Identifier: string(source[replacement.Start:replacement.End]), Replacement: replacement}, true
}

func enclosingMetaModel(syntax []SyntaxStatement, offset int, filePath string, graph *schema.Graph) (string, bool) {
	var meta SyntaxStatement
	for _, statement := range syntax {
		if !statement.ScopeMarker || statement.ScopeKind != "class_definition" || statement.ScopeStart > offset || offset > statement.ScopeEnd || definitionName(statement.Text) != "Meta" {
			continue
		}
		if meta.ScopeEnd == 0 || statement.ScopeEnd-statement.ScopeStart < meta.ScopeEnd-meta.ScopeStart {
			meta = statement
		}
	}
	if meta.ScopeEnd == 0 {
		return "", false
	}
	var parent SyntaxStatement
	for _, statement := range syntax {
		if !statement.ScopeMarker || statement.ScopeKind != "class_definition" || statement.Start == meta.Start && statement.End == meta.End || statement.ScopeStart > meta.ScopeStart || meta.ScopeEnd > statement.ScopeEnd {
			continue
		}
		if parent.ScopeEnd == 0 || statement.ScopeEnd-statement.ScopeStart < parent.ScopeEnd-parent.ScopeStart {
			parent = statement
		}
	}
	if parent.ScopeEnd == 0 {
		return "", false
	}
	return graph.CanonicalLabelForSourceClass(filePath, definitionName(parent.Text))
}

func definitionName(text string) string {
	match := definitionBindingPattern.FindStringSubmatch(text)
	if match == nil {
		return ""
	}
	return match[1]
}

func metaFieldStringRange(source []byte, offset int) (ByteRange, bool) {
	for start := offset - 1; start >= 0; start-- {
		if source[start] != '\'' && source[start] != '"' || start > 0 && source[start-1] == '\\' {
			continue
		}
		quote := source[start]
		end := start + 1
		for end < len(source) && (source[end] != quote || source[end-1] == '\\') && source[end] != '\n' {
			end++
		}
		if end < len(source) && source[end] == quote && start < offset && offset <= end {
			return ByteRange{Start: start + 1, End: end}, true
		}
	}
	return ByteRange{}, false
}

func metaFieldOption(source []byte, stringStart int) bool {
	prefix := string(source[:stringStart])
	option := ""
	optionStart := -1
	for _, candidate := range []string{"fields", "ordering", "unique_together", "index_together", "get_latest_by", "order_with_respect_to"} {
		index := strings.LastIndex(prefix, candidate)
		if index < 0 || index+len(candidate) >= len(prefix) || !isSpaceOrEquals(prefix[index+len(candidate)]) {
			continue
		}
		if index > optionStart {
			option = candidate
			optionStart = index
		}
	}
	if option == "" {
		return false
	}
	assignment := strings.IndexByte(prefix[optionStart+len(option):], '=')
	if assignment < 0 {
		return false
	}
	assignment += optionStart + len(option)
	if option == "get_latest_by" || option == "order_with_respect_to" {
		return strings.TrimSpace(prefix[assignment+1:]) == ""
	}
	return metaListOpen(source, assignment+1, stringStart)
}

func metaListOpen(source []byte, start, end int) bool {
	depth := 0
	code := pythonCodeMask(source, end)
	for index := start; index < end; index++ {
		if !code[index] {
			continue
		}
		switch source[index] {
		case '[':
			depth++
		case ']':
			if depth > 0 {
				depth--
			}
		}
	}
	return depth > 0
}

func isSpaceOrEquals(value byte) bool {
	return value == '=' || value == ' ' || value == '\t' || value == '\n'
}

func queryReceiver(source []byte, before int) (ByteRange, int, bool) {
	code := pythonCodeMask(source, before)
	stack := make([]int, 0, 8)
	for index := 0; index < before; index++ {
		if !code[index] {
			continue
		}
		switch source[index] {
		case '(':
			stack = append(stack, index)
		case ')':
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		}
	}
	if len(stack) == 0 {
		return ByteRange{}, 0, false
	}
	opening := stack[len(stack)-1]
	nameEnd := opening
	for nameEnd > 0 && (source[nameEnd-1] == ' ' || source[nameEnd-1] == '\t') {
		nameEnd--
	}
	nameStart := nameEnd
	for nameStart > 0 && isIdentifierByte(source[nameStart-1]) && code[nameStart-1] {
		nameStart--
	}
	method := string(source[nameStart:nameEnd])
	if method != "filter" && method != "exclude" && method != "get" && method != "get_or_create" && method != "update_or_create" && method != "create" && method != "update" {
		return ByteRange{}, 0, false
	}
	if nameStart == 0 || source[nameStart-1] != '.' || !code[nameStart-1] {
		return ByteRange{}, 0, false
	}
	return expressionBefore(source, nameStart-1), opening + 1, true
}

func pythonCodeMask(source []byte, end int) []bool {
	if end > len(source) {
		end = len(source)
	}
	code := make([]bool, end)
	var quote byte
	triple := false
	escaped := false
	comment := false
	for index := 0; index < end; index++ {
		value := source[index]
		if comment {
			if value == '\n' {
				comment = false
				code[index] = true
			}
			continue
		}
		if quote != 0 {
			if triple {
				if value == quote && index+2 < end && source[index+1] == quote && source[index+2] == quote {
					quote = 0
					triple = false
					index += 2
				}
				continue
			}
			if escaped {
				escaped = false
				continue
			}
			if value == '\\' {
				escaped = true
			} else if value == quote {
				quote = 0
			}
			continue
		}
		if value == '#' {
			comment = true
			continue
		}
		if value == '\'' || value == '"' {
			quote = value
			if index+2 < end && source[index+1] == value && source[index+2] == value {
				triple = true
				index += 2
			}
			continue
		}
		code[index] = true
	}
	return code
}

func keywordNamePosition(source []byte, argumentsStart, identifierStart int) bool {
	if argumentsStart < 0 || identifierStart < argumentsStart || identifierStart > len(source) {
		return false
	}
	segmentStart := argumentsStart
	depth := 0
	var quote byte
	escaped := false
	comment := false
	for index := argumentsStart; index < identifierStart; index++ {
		value := source[index]
		if comment {
			if value == '\n' {
				comment = false
			}
			continue
		}
		if quote != 0 {
			if escaped {
				escaped = false
				continue
			}
			if value == '\\' {
				escaped = true
			} else if value == quote {
				quote = 0
			}
			continue
		}
		switch value {
		case '\'', '"':
			quote = value
		case '#':
			comment = true
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			if depth == 0 {
				return false
			}
			depth--
		case ',':
			if depth == 0 {
				segmentStart = index + 1
			}
		}
	}
	return depth == 0 && quote == 0 && !comment && strings.TrimSpace(string(source[segmentStart:identifierStart])) == ""
}

func expressionBefore(source []byte, end int) ByteRange {
	start := end
	for start > 0 && isSpace(source[start-1]) {
		start--
	}
	depth := 0
	for start > 0 {
		value := source[start-1]
		switch value {
		case ')', ']':
			depth++
			start--
		case '(', '[':
			if depth == 0 {
				return ByteRange{Start: start, End: end}
			}
			depth--
			start--
		default:
			if depth == 0 && isSpace(value) {
				spaceStart := start - 1
				for spaceStart > 0 && isSpace(source[spaceStart-1]) {
					spaceStart--
				}
				if start < end && source[start] == '.' && spaceStart > 0 && source[spaceStart-1] == ')' {
					start = spaceStart
					continue
				}
			}
			if depth == 0 && !isIdentifierByte(value) && value != '.' {
				return ByteRange{Start: start, End: end}
			}
			start--
		}
	}
	return ByteRange{Start: start, End: end}
}

func identifierRange(source []byte, offset int) ByteRange {
	start := offset
	for start > 0 {
		rune_, size := utf8.DecodeLastRune(source[:start])
		if rune_ == utf8.RuneError && size == 1 || !isIdentifierRune(rune_) {
			break
		}
		start -= size
	}
	end := offset
	for end < len(source) {
		rune_, size := utf8.DecodeRune(source[end:])
		if rune_ == utf8.RuneError && size == 1 || !isIdentifierRune(rune_) {
			break
		}
		end += size
	}
	return ByteRange{Start: start, End: end}
}

func isIdentifierRune(value rune) bool {
	return value == '_' || unicode.IsLetter(value) || unicode.IsDigit(value) || unicode.IsMark(value)
}

func isIdentifierByte(value byte) bool {
	return value == '_' || value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9'
}

func inferExpression(expression string, sourcePrefix []byte, graph *schema.Graph, syntax []SyntaxStatement, offset int) Value {
	return inferExpressionAtPath(expression, sourcePrefix, graph, syntax, offset, "")
}

func inferExpressionAtPath(expression string, sourcePrefix []byte, graph *schema.Graph, syntax []SyntaxStatement, offset int, filePath string) Value {
	imports, values := buildEnvironmentAtPath(sourcePrefix, graph, syntax, offset, filePath)
	if label, name, ok := enclosingModelClass(syntax, offset, filePath, graph); ok {
		if _, imported := imports[name]; !imported {
			if _, bound := values[name]; !bound {
				values[name] = Value{CanonicalLabel: label, Kind: ValueModelClass}
			}
		}
	}
	if rewritten, ok := rewriteAdminQuerySetExpression(expression); ok {
		if label, registered := enclosingAdminModelLabel(sourcePrefix, syntax, offset, imports, graph); registered {
			values[adminQuerySetBinding] = Value{CanonicalLabel: label, Kind: ValueQuerySet}
			expression = rewritten
		}
	}
	return resolveExpression(expression, imports, values, graph)
}

func enclosingModelClass(syntax []SyntaxStatement, offset int, filePath string, graph *schema.Graph) (string, string, bool) {
	if graph == nil || filePath == "" {
		return "", "", false
	}
	var selected SyntaxStatement
	for _, statement := range syntax {
		if !statement.ScopeMarker || statement.ScopeKind != "class_definition" || statement.ScopeStart > offset || offset > statement.ScopeEnd {
			continue
		}
		if selected.ScopeEnd == 0 || statement.ScopeEnd-statement.ScopeStart < selected.ScopeEnd-selected.ScopeStart {
			selected = statement
		}
	}
	if selected.ScopeEnd == 0 {
		return "", "", false
	}
	match := definitionBindingPattern.FindStringSubmatch(selected.Text)
	if match == nil {
		return "", "", false
	}
	label, ok := graph.CanonicalLabelForSourceClass(filePath, match[1])
	return label, match[1], ok
}

func buildEnvironment(source []byte, graph *schema.Graph, syntax []SyntaxStatement, offset int) (map[string]string, map[string]Value) {
	return buildEnvironmentAtPath(source, graph, syntax, offset, "")
}

type forBinding struct {
	variable string
	iterable string
}

func buildEnvironmentAtPath(source []byte, graph *schema.Graph, syntax []SyntaxStatement, offset int, filePath string) (map[string]string, map[string]Value) {
	imports := make(map[string]string)
	values := make(map[string]Value)
	var guardedBindings []string
	var futureBindings []string
	var opaqueBindings []string
	var pendingForBindings []forBinding
	lines := bytes.Split(source, []byte{'\n'})
	if len(syntax) > 0 {
		ordered := append([]SyntaxStatement(nil), syntax...)
		sort.SliceStable(ordered, func(left, right int) bool { return ordered[left].Start < ordered[right].Start })
		currentScope := SyntaxStatement{}
		for _, statement := range ordered {
			if !statement.ScopeMarker || statement.ScopeStart > offset || offset > statement.ScopeEnd {
				continue
			}
			if currentScope.ScopeEnd == 0 || statement.ScopeEnd-statement.ScopeStart < currentScope.ScopeEnd-currentScope.ScopeStart {
				currentScope = statement
			}
		}
		lines = lines[:0]
		for _, statement := range ordered {
			if statement.ScopeMarker {
				if statement.Start == currentScope.Start && statement.End == currentScope.End || currentScope.ScopeEnd != 0 && (statement.Start <= currentScope.ScopeStart || statement.End > currentScope.ScopeEnd) {
					continue
				}
				if currentScope.ScopeKind == "function_definition" || statement.End <= offset {
					if match := definitionBindingPattern.FindStringSubmatch(statement.Text); match != nil {
						opaqueBindings = append(opaqueBindings, match[1])
					}
				}
				continue
			}
			inScope := statement.ScopeKind == "" || statement.ScopeStart == currentScope.ScopeStart && statement.ScopeEnd == currentScope.ScopeEnd && statement.ScopeKind == currentScope.ScopeKind
			if statement.Guarded {
				if inScope && (statement.Start < offset || currentScope.ScopeKind == "function_definition") {
					// If the cursor is inside a for-in statement, infer the loop variable from the iterable.
					if statement.Start < offset && offset <= statement.End {
						firstLine := statement.Text
						if nl := strings.IndexByte(firstLine, '\n'); nl >= 0 {
							firstLine = firstLine[:nl]
						}
						if match := forHeaderPattern.FindStringSubmatch(firstLine); match != nil {
							pendingForBindings = append(pendingForBindings, forBinding{variable: match[1], iterable: strings.TrimSpace(match[2])})
							continue
						}
					}
					guardedBindings = append(guardedBindings, statementBindingNames(statement.Text)...)
				}
				continue
			}
			if statement.End > offset && inScope && currentScope.ScopeKind == "function_definition" {
				futureBindings = append(futureBindings, statementBindingNames(statement.Text)...)
				continue
			}
			if statement.End <= offset && inScope {
				lines = append(lines, []byte(statement.Text))
				if !recognizedEnvironmentStatement(statement.Text) {
					opaqueBindings = append(opaqueBindings, statementBindingNames(statement.Text)...)
				}
			}
		}
	}
	for _, rawLine := range lines {
		line := strings.TrimSpace(strings.TrimSuffix(string(rawLine), "\r"))
		if match := fromImportPattern.FindStringSubmatch(line); match != nil {
			items := strings.Trim(strings.TrimSpace(match[2]), "()")
			for _, item := range strings.Split(items, ",") {
				parts := strings.Fields(strings.TrimSpace(item))
				if len(parts) == 0 || parts[0] == "*" {
					continue
				}
				local := parts[0]
				if len(parts) == 3 && parts[1] == "as" {
					local = parts[2]
				}
				imports[local] = match[1] + "." + parts[0]
			}
			continue
		}
		if match := importPattern.FindStringSubmatch(line); match != nil {
			local := strings.Split(match[1], ".")[0]
			imported := local
			if match[2] != "" {
				local = match[2]
				imported = match[1]
			}
			imports[local] = imported
			continue
		}
		if match := annotationPattern.FindStringSubmatch(line); match != nil {
			name := match[1]
			if match[3] != "" {
				if value := resolveExpression(match[3], imports, values, graph); value.Kind != ValueUnknown {
					values[name] = value
					delete(imports, name)
					continue
				}
			}
			annotation := match[2]
			kind := ValueModelInstance
			if open := strings.IndexByte(annotation, '['); open >= 0 && strings.HasSuffix(annotation, "]") {
				container := annotation[:open]
				container = expandImport(container, imports)
				if container != "QuerySet" && !strings.HasSuffix(container, ".QuerySet") {
					values[name] = Value{}
					delete(imports, name)
					continue
				}
				annotation = annotation[open+1 : len(annotation)-1]
				kind = ValueQuerySet
			}
			if label, ok := resolveClass(annotation, imports, graph); ok {
				values[name] = Value{CanonicalLabel: label, Kind: kind}
			} else {
				values[name] = Value{}
			}
			delete(imports, name)
			continue
		}
		if match := forHeaderPattern.FindStringSubmatch(line); match != nil {
			iterable := resolveExpression(strings.TrimSpace(match[2]), imports, values, graph)
			name := match[1]
			delete(imports, name)
			if iterable.Kind == ValueQuerySet {
				values[name] = Value{CanonicalLabel: iterable.CanonicalLabel, Kind: ValueModelInstance, Annotations: iterable.Annotations}
			} else {
				values[name] = Value{}
			}
			continue
		}
		if match := assignmentPattern.FindStringSubmatch(line); match != nil {
			value := Value{}
			if match[1] == "self" && strings.TrimSpace(match[2]) == "__pogo_model_receiver__" {
				if label, ok := enclosingModelLabel(syntax, offset, filePath, graph); ok {
					value = Value{CanonicalLabel: label, Kind: ValueModelInstance}
				}
			} else if shortcut, ok := djangoShortcutValue(match[2], imports, graph); ok {
				value = shortcut
			} else {
				value = resolveExpression(match[2], imports, values, graph)
			}
			delete(imports, match[1])
			values[match[1]] = value
		}
	}
	for _, binding := range pendingForBindings {
		delete(imports, binding.variable)
		delete(values, binding.variable)
		iterable := resolveExpression(binding.iterable, imports, values, graph)
		if iterable.Kind == ValueQuerySet {
			values[binding.variable] = Value{CanonicalLabel: iterable.CanonicalLabel, Kind: ValueModelInstance, Annotations: iterable.Annotations}
		}
	}
	for _, name := range guardedBindings {
		delete(imports, name)
		delete(values, name)
	}
	for _, name := range futureBindings {
		delete(imports, name)
		delete(values, name)
	}
	for _, name := range opaqueBindings {
		delete(imports, name)
		delete(values, name)
	}
	return imports, values
}

func djangoShortcutValue(expression string, imports map[string]string, graph *schema.Graph) (Value, bool) {
	expression = strings.TrimSpace(expression)
	opening := strings.IndexByte(expression, '(')
	if opening <= 0 || len(expression) > MaxPathBytes {
		return Value{}, false
	}
	function := strings.TrimSpace(expression[:opening])
	if !identifierText(strings.ReplaceAll(function, ".", "")) || expandImport(function, imports) != "django.shortcuts.get_object_or_404" {
		return Value{}, false
	}
	end, ok := skipCall(expression, opening)
	if !ok || strings.TrimSpace(expression[end:]) != "" {
		return Value{}, false
	}
	argument := firstCallArgument(expression[opening+1 : end-1])
	label, ok := resolveClass(strings.TrimSpace(argument), imports, graph)
	if !ok {
		return Value{}, false
	}
	return Value{CanonicalLabel: label, Kind: ValueModelInstance}, true
}

func firstCallArgument(arguments string) string {
	depth := 0
	var quote byte
	escaped := false
	for index := 0; index < len(arguments); index++ {
		value := arguments[index]
		if quote != 0 {
			if escaped {
				escaped = false
			} else if value == '\\' {
				escaped = true
			} else if value == quote {
				quote = 0
			}
			continue
		}
		switch value {
		case '\'', '"':
			quote = value
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				return arguments[:index]
			}
		}
	}
	return arguments
}

func enclosingModelLabel(syntax []SyntaxStatement, offset int, filePath string, graph *schema.Graph) (string, bool) {
	if graph == nil || filePath == "" {
		return "", false
	}
	current, parent, ok := enclosingFunctionAndClass(syntax, offset)
	if !ok {
		return "", false
	}
	var classes []SyntaxStatement
	for _, statement := range syntax {
		if statement.ScopeMarker && statement.ScopeKind == "class_definition" && statement.ScopeStart <= current.ScopeStart && current.ScopeEnd <= statement.ScopeEnd {
			classes = append(classes, statement)
		}
	}
	sort.Slice(classes, func(left, right int) bool {
		if classes[left].ScopeStart != classes[right].ScopeStart {
			return classes[left].ScopeStart < classes[right].ScopeStart
		}
		return classes[left].ScopeEnd > classes[right].ScopeEnd
	})
	qualname := make([]string, 0, len(classes))
	for _, class := range classes {
		match := definitionBindingPattern.FindStringSubmatch(class.Text)
		if match == nil {
			return "", false
		}
		qualname = append(qualname, match[1])
	}
	if len(qualname) == 0 || parent.ScopeKind != "class_definition" {
		return "", false
	}
	return graph.CanonicalLabelForSourceClass(filePath, strings.Join(qualname, "."))
}

func enclosingFunctionAndClass(syntax []SyntaxStatement, offset int) (SyntaxStatement, SyntaxStatement, bool) {
	current := SyntaxStatement{}
	for _, statement := range syntax {
		if !statement.ScopeMarker || statement.ScopeStart > offset || offset > statement.ScopeEnd {
			continue
		}
		if current.ScopeEnd == 0 || statement.ScopeEnd-statement.ScopeStart < current.ScopeEnd-current.ScopeStart {
			current = statement
		}
	}
	if current.ScopeKind != "function_definition" {
		return SyntaxStatement{}, SyntaxStatement{}, false
	}
	parent := SyntaxStatement{}
	for _, statement := range syntax {
		if !statement.ScopeMarker || statement.Start == current.Start && statement.End == current.End || statement.ScopeStart > current.ScopeStart || current.ScopeEnd > statement.ScopeEnd {
			continue
		}
		if parent.ScopeEnd == 0 || statement.ScopeEnd-statement.ScopeStart < parent.ScopeEnd-parent.ScopeStart {
			parent = statement
		}
	}
	if parent.ScopeKind != "class_definition" {
		return SyntaxStatement{}, SyntaxStatement{}, false
	}
	return current, parent, true
}

func enclosingAdminModelLabel(source []byte, syntax []SyntaxStatement, offset int, imports map[string]string, graph *schema.Graph) (string, bool) {
	current, class, ok := enclosingFunctionAndClass(syntax, offset)
	if !ok || graph == nil || class.ScopeStart < 0 || class.ScopeStart > len(source) {
		return "", false
	}
	function := definitionBindingPattern.FindStringSubmatch(current.Text)
	if function == nil || function[1] != "get_queryset" {
		return "", false
	}
	cursor := bytes.LastIndexByte(source[:class.ScopeStart], '\n') + 1
	for cursor > 0 {
		lineEnd := cursor - 1
		lineStart := bytes.LastIndexByte(source[:lineEnd], '\n') + 1
		line := strings.TrimSpace(string(source[lineStart:lineEnd]))
		if !strings.HasPrefix(line, "@") {
			break
		}
		match := adminRegisterPattern.FindStringSubmatch(line)
		if match != nil && expandImport(match[1], imports) == "django.contrib.admin.register" {
			return resolveClass(match[2], imports, graph)
		}
		cursor = lineStart
	}
	return "", false
}

func rewriteAdminQuerySetExpression(expression string) (string, bool) {
	position := skipExpressionSpace(expression, 0)
	if !strings.HasPrefix(expression[position:], "super") {
		return "", false
	}
	position += len("super")
	position = skipExpressionSpace(expression, position)
	if position >= len(expression) || expression[position] != '(' {
		return "", false
	}
	end, ok := skipCall(expression, position)
	if !ok || strings.TrimSpace(expression[position+1:end-1]) != "" {
		return "", false
	}
	position = skipExpressionSpace(expression, end)
	if position >= len(expression) || expression[position] != '.' {
		return "", false
	}
	position++
	position = skipExpressionSpace(expression, position)
	const method = "get_queryset"
	if !strings.HasPrefix(expression[position:], method) {
		return "", false
	}
	position += len(method)
	position = skipExpressionSpace(expression, position)
	if position >= len(expression) || expression[position] != '(' {
		return "", false
	}
	end, ok = skipCall(expression, position)
	if !ok {
		return "", false
	}
	return adminQuerySetBinding + expression[end:], true
}

func importBindingNames(line string) []string {
	line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
	if match := fromImportPattern.FindStringSubmatch(line); match != nil {
		items := strings.Trim(strings.TrimSpace(match[2]), "()")
		var names []string
		for _, item := range strings.Split(items, ",") {
			parts := strings.Fields(strings.TrimSpace(item))
			if len(parts) == 0 || parts[0] == "*" {
				continue
			}
			name := parts[0]
			if len(parts) == 3 && parts[1] == "as" {
				name = parts[2]
			}
			names = append(names, name)
		}
		return names
	}
	if match := importPattern.FindStringSubmatch(line); match != nil {
		name := strings.Split(match[1], ".")[0]
		if match[2] != "" {
			name = match[2]
		}
		return []string{name}
	}
	return nil
}

func statementBindingNames(text string) []string {
	seen := make(map[string]struct{})
	var names []string
	addTarget := func(target string) {
		for _, name := range bindingIdentifierPattern.FindAllString(target, -1) {
			if _, exists := seen[name]; !exists {
				seen[name] = struct{}{}
				names = append(names, name)
			}
		}
	}
	for _, name := range importBindingNames(text) {
		addTarget(name)
	}
	if match := definitionBindingPattern.FindStringSubmatch(text); match != nil {
		addTarget(match[1])
	}
	if match := annotationPattern.FindStringSubmatch(strings.TrimSpace(text)); match != nil {
		addTarget(match[1])
	} else {
		for _, target := range assignmentTargets(text) {
			for _, name := range bindingTargetNames(target) {
				addTarget(name)
			}
		}
	}
	for _, match := range forBindingPattern.FindAllStringSubmatch(text, -1) {
		for _, name := range bindingTargetNames(match[1]) {
			addTarget(name)
		}
	}
	for _, match := range asBindingPattern.FindAllStringSubmatch(text, -1) {
		for _, name := range bindingTargetNames(match[1]) {
			addTarget(name)
		}
	}
	for _, match := range walrusBindingPattern.FindAllStringSubmatch(text, -1) {
		addTarget(match[1])
	}
	return names
}

func assignmentTargets(text string) []string {
	var targets []string
	start := 0
	depth := 0
	var quote byte
	escaped := false
	for index := 0; index < len(text); index++ {
		value := text[index]
		if quote != 0 {
			if escaped {
				escaped = false
			} else if value == '\\' {
				escaped = true
			} else if value == quote {
				quote = 0
			}
			continue
		}
		switch value {
		case '\'', '"':
			quote = value
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			if depth > 0 {
				depth--
			}
		case '=':
			if depth == 0 && (index == 0 || text[index-1] != '=' && !strings.ContainsRune("!<>", rune(text[index-1]))) && (index+1 == len(text) || text[index+1] != '=') {
				targets = append(targets, text[start:index])
				start = index + 1
			}
		}
	}
	return targets
}

func bindingTargetNames(target string) []string {
	target = strings.TrimSpace(target)
	for strings.HasPrefix(target, "*") {
		target = strings.TrimSpace(target[1:])
	}
	if colon := strings.IndexByte(target, ':'); colon >= 0 {
		target = strings.TrimSpace(target[:colon])
	}
	if len(target) >= 2 && (target[0] == '(' && target[len(target)-1] == ')' || target[0] == '[' && target[len(target)-1] == ']') {
		target = target[1 : len(target)-1]
	}
	parts := splitBindingTargets(target)
	if len(parts) > 1 {
		var names []string
		for _, part := range parts {
			names = append(names, bindingTargetNames(part)...)
		}
		return names
	}
	if identifierText(target) {
		return []string{target}
	}
	return nil
}

func splitBindingTargets(target string) []string {
	start := 0
	depth := 0
	var parts []string
	for index := 0; index < len(target); index++ {
		switch target[index] {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				parts = append(parts, target[start:index])
				start = index + 1
			}
		}
	}
	if start > 0 {
		parts = append(parts, target[start:])
	}
	return parts
}

func recognizedEnvironmentStatement(text string) bool {
	line := strings.TrimSpace(strings.TrimSuffix(text, "\r"))
	return fromImportPattern.MatchString(line) || importPattern.MatchString(line) || annotationPattern.MatchString(line) || assignmentPattern.MatchString(line) || forHeaderPattern.MatchString(line)
}

func resolveExpression(expression string, imports map[string]string, values map[string]Value, graph *schema.Graph) Value {
	expression = strings.TrimSpace(expression)
	if len(expression) > MaxPathBytes {
		return Value{}
	}
	for strings.HasPrefix(expression, "(") {
		end, ok := skipCall(expression, 0)
		if !ok || end != len(expression) {
			break
		}
		expression = strings.TrimSpace(expression[1 : len(expression)-1])
	}
	if expression == "" {
		return Value{}
	}
	if value, exists := values[expression]; exists {
		return value
	}
	baseEnd := 0
	for baseEnd < len(expression) && (isIdentifierByte(expression[baseEnd]) || expression[baseEnd] == '.') {
		baseEnd++
	}
	reference := strings.TrimSuffix(expression[:baseEnd], ".")
	var value Value
	consumed := 0
	for boundary := len(reference); boundary > 0; {
		candidate := reference[:boundary]
		if label, ok := resolveClass(candidate, imports, graph); ok {
			value = Value{CanonicalLabel: label, Kind: ValueModelClass}
			consumed = boundary
			break
		}
		previous := strings.LastIndexByte(candidate, '.')
		if previous < 0 {
			break
		}
		boundary = previous
	}
	if value.Kind == ValueUnknown {
		firstEnd := strings.IndexByte(reference, '.')
		if firstEnd < 0 {
			firstEnd = len(reference)
		}
		base := reference[:firstEnd]
		value = values[base]
		consumed = firstEnd
	}
	if value.Kind == ValueUnknown {
		return Value{}
	}
	position := consumed
	position = skipExpressionSpace(expression, position)
	if position < len(expression) && expression[position] == '(' {
		end, ok := skipCall(expression, position)
		if !ok || value.Kind != ValueModelClass {
			return Value{}
		}
		value.Kind = ValueModelInstance
		position = end
	}
	segments := 0
	for position < len(expression) {
		segments++
		if segments > MaxPathSegments {
			return Value{}
		}
		position = skipExpressionSpace(expression, position)
		if position == len(expression) {
			return value
		}
		if expression[position] != '.' {
			if strings.TrimSpace(expression[position:]) == "" {
				return value
			}
			return Value{}
		}
		position++
		position = skipExpressionSpace(expression, position)
		nameStart := position
		for position < len(expression) && isIdentifierByte(expression[position]) {
			position++
		}
		if nameStart == position {
			return Value{}
		}
		name := expression[nameStart:position]
		called := false
		callOpen := -1
		position = skipExpressionSpace(expression, position)
		if position < len(expression) && expression[position] == '(' {
			callOpen = position
			end, ok := skipCall(expression, position)
			if !ok {
				return Value{}
			}
			called = true
			position = end
		}
		switch value.Kind {
		case ValueModelClass:
			if called {
				return Value{}
			}
			manager, ok := graph.Manager(value.CanonicalLabel, name)
			if !ok {
				return Value{}
			}
			value.Kind = ValueManager
			value.ManagerName = name
			if class, ok := manager.QuerySetClass(); ok {
				value.QuerySetClass = class
			}
		case ValueManager, ValueQuerySet:
			if !called {
				return Value{}
			}
			if method, ok := methodForValue(graph, value, name); ok {
				if !method.Chainable() {
					return Value{}
				}
				if value.Kind == ValueManager {
					value.ManagerName = ""
				}
				value.Kind = ValueQuerySet
				break
			}
			switch name {
			case "get", "first", "last", "create":
				annotations := value.Annotations
				value.Kind = ValueModelInstance
				value.ManagerName = ""
				value.QuerySetClass = ""
				value.Annotations = annotations
			case "annotate", "alias":
				if value.Kind == ValueManager {
					value.ManagerName = ""
				}
				value.Kind = ValueQuerySet
				if callOpen >= 0 && position > callOpen+1 {
					newAliases := annotationKeywordNames(expression[callOpen+1 : position-1])
					if len(newAliases) > 0 {
						merged := make([]string, len(value.Annotations)+len(newAliases))
						copy(merged, value.Annotations)
						copy(merged[len(value.Annotations):], newAliases)
						value.Annotations = merged
					}
				}
			case "all", "filter", "exclude", "distinct", "order_by", "values", "values_list", "only", "defer", "select_related", "prefetch_related":
				if value.Kind == ValueManager {
					value.ManagerName = ""
				}
				value.Kind = ValueQuerySet
			default:
				return Value{}
			}
		case ValueModelInstance:
			if called {
				return Value{}
			}
			access, ok := graph.InstanceAccess(value.CanonicalLabel, name)
			if !ok || access.Field == nil {
				return Value{}
			}
			if label, ok := singleRelatedInstance(access.Field); ok {
				value = Value{CanonicalLabel: label, Kind: ValueModelInstance}
				break
			}
			label, ok := relatedManagerModel(access.Field)
			if !ok {
				return Value{}
			}
			value = Value{CanonicalLabel: label, Kind: ValueManager}
		default:
			return Value{}
		}
	}
	return value
}

// annotationKeywordNames extracts keyword argument names from a call's argument string.
// For example, "has_books=Exists(qs), count=Count('id')" returns ["has_books", "count"].
func annotationKeywordNames(args string) []string {
	var names []string
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
				if name, ok := keywordArgName(args[segStart:index]); ok {
					names = append(names, name)
				}
				segStart = index + 1
			}
		}
	}
	return names
}

func keywordArgName(segment string) (string, bool) {
	segment = strings.TrimSpace(segment)
	for index := 0; index < len(segment); index++ {
		ch := segment[index]
		switch ch {
		case '(', '[', '{':
			return "", false
		case '=':
			if index > 0 {
				prev := segment[index-1]
				if prev != '!' && prev != '<' && prev != '>' {
					next := byte(0)
					if index+1 < len(segment) {
						next = segment[index+1]
					}
					if next != '=' {
						name := strings.TrimSpace(segment[:index])
						if identifierText(name) {
							return name, true
						}
					}
				}
			}
			return "", false
		}
	}
	return "", false
}

func relatedManagerModel(field *schema.FieldRef) (string, bool) {
	if field == nil || !field.IsRelation() {
		return "", false
	}
	cardinality, ok := field.RelationCardinality()
	if !ok || cardinality != schema.RelationOneToMany && cardinality != schema.RelationManyToMany {
		return "", false
	}
	return field.RelatedModel()
}

func singleRelatedInstance(field *schema.FieldRef) (string, bool) {
	if field == nil || !field.IsRelation() {
		return "", false
	}
	cardinality, ok := field.RelationCardinality()
	if !ok {
		return "", false
	}
	if cardinality != schema.RelationOneToOne {
		direction, hasDirection := field.RelationDirection()
		if cardinality != schema.RelationManyToOne || !hasDirection || direction != schema.RelationForward {
			return "", false
		}
	}
	return field.RelatedModel()
}

func methodForValue(graph *schema.Graph, value Value, name string) (*schema.MethodRef, bool) {
	if graph == nil {
		return nil, false
	}
	if value.Kind == ValueManager {
		manager, ok := graph.Manager(value.CanonicalLabel, value.ManagerName)
		if !ok {
			return nil, false
		}
		if method, ok := manager.Method(name); ok {
			return method, true
		}
		return manager.QuerySetMethod(name)
	}
	if value.Kind == ValueQuerySet && value.QuerySetClass != "" {
		return graph.QuerySetMethod(value.CanonicalLabel, value.QuerySetClass, name)
	}
	return nil, false
}

func ResolveMethod(graph *schema.Graph, value Value, name string) (*schema.MethodRef, bool) {
	return methodForValue(graph, value, name)
}

func VisitMethods(graph *schema.Graph, value Value, visit func(*schema.MethodRef) bool) bool {
	if graph == nil {
		return false
	}
	seen := make(map[string]struct{})
	visitOnce := func(method *schema.MethodRef) bool {
		key := method.Name()
		if _, exists := seen[key]; exists {
			return true
		}
		seen[key] = struct{}{}
		return visit(method)
	}
	if value.Kind == ValueManager {
		manager, ok := graph.Manager(value.CanonicalLabel, value.ManagerName)
		if !ok {
			return false
		}
		if !manager.VisitMethods(visitOnce) {
			return false
		}
		return manager.VisitQuerySetMethods(visitOnce)
	}
	if value.Kind == ValueQuerySet && value.QuerySetClass != "" {
		querySet, ok := graph.QuerySet(value.CanonicalLabel, value.QuerySetClass)
		if !ok {
			return false
		}
		return querySet.VisitMethods(visitOnce)
	}
	return false
}

func methodIf(ok bool, method *schema.MethodRef) *schema.MethodRef {
	if !ok {
		return nil
	}
	return method
}

func resolveClass(reference string, imports map[string]string, graph *schema.Graph) (string, bool) {
	return graph.CanonicalLabelForClass(expandImport(reference, imports))
}

func expandImport(reference string, imports map[string]string) string {
	full := reference
	first, rest, hasRest := strings.Cut(reference, ".")
	if imported, exists := imports[first]; exists {
		full = imported
		if hasRest {
			full += "." + rest
		}
	} else if imported, exists := imports[reference]; exists {
		full = imported
	}
	return full
}

func skipCall(expression string, start int) (int, bool) {
	depth := 0
	var quote byte
	escaped := false
	comment := false
	for index := start; index < len(expression); index++ {
		value := expression[index]
		if comment {
			if value == '\n' {
				comment = false
			}
			continue
		}
		if quote != 0 {
			if escaped {
				escaped = false
				continue
			}
			if value == '\\' {
				escaped = true
			} else if value == quote {
				quote = 0
			}
			continue
		}
		if value == '#' {
			comment = true
			continue
		}
		if value == '\'' || value == '"' {
			quote = value
			continue
		}
		switch value {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return index + 1, true
			}
		}
	}
	return len(expression), false
}

func skipExpressionSpace(expression string, position int) int {
	for position < len(expression) {
		switch expression[position] {
		case ' ', '\t', '\r', '\n':
			position++
		default:
			return position
		}
	}
	return position
}
