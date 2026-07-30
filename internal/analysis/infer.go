package analysis

import (
	"bytes"
	"regexp"
	"sort"
	"strings"

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
}

type ContextKind uint8

const (
	ContextUnknown ContextKind = iota
	ContextQueryKeyword
	ContextModelMember
	ContextInstanceMember
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
}

var (
	fromImportPattern = regexp.MustCompile(`(?s)^\s*from\s+([A-Za-z_][A-Za-z0-9_.]*)\s+import\s+(.+)$`)
	importPattern     = regexp.MustCompile(`^\s*import\s+([A-Za-z_][A-Za-z0-9_.]*)(?:\s+as\s+([A-Za-z_][A-Za-z0-9_]*))?\s*$`)
	annotationPattern = regexp.MustCompile(`(?s)^\s*([A-Za-z_][A-Za-z0-9_]*)\s*:\s*([A-Za-z_][A-Za-z0-9_.]*(?:\[[A-Za-z_][A-Za-z0-9_.]*\])?)(?:\s*=\s*(.+))?$`)
	assignmentPattern = regexp.MustCompile(`(?s)^\s*([A-Za-z_][A-Za-z0-9_]*)\s*=\s*(.+)$`)
)

func Analyze(source []byte, offset int, graph *schema.Graph) (Context, bool) {
	return analyze(source, offset, graph, nil)
}

func AnalyzeSyntax(source []byte, offset int, graph *schema.Graph, syntax []SyntaxStatement) (Context, bool) {
	return analyze(source, offset, graph, syntax)
}

func analyze(source []byte, offset int, graph *schema.Graph, syntax []SyntaxStatement) (Context, bool) {
	if graph == nil || offset < 0 || offset > len(source) {
		return Context{}, false
	}
	replacement := identifierRange(source, offset)
	identifier := string(source[replacement.Start:replacement.End])
	if strings.Contains(identifier, "__") {
		return Context{}, false
	}
	if receiver, argumentsStart, ok := queryReceiver(source, replacement.Start); ok && keywordNamePosition(source, argumentsStart, replacement.Start) {
		value := inferExpression(strings.TrimSpace(string(source[receiver.Start:receiver.End])), source[:offset], graph, syntax, offset)
		if value.Kind == ValueManager || value.Kind == ValueQuerySet {
			return Context{Kind: ContextQueryKeyword, Value: value, Identifier: identifier, Replacement: replacement}, true
		}
	}
	if replacement.Start == 0 || source[replacement.Start-1] != '.' {
		return Context{}, false
	}
	receiver := expressionBefore(source, replacement.Start-1)
	if receiver.Start == receiver.End {
		return Context{}, false
	}
	value := inferExpression(strings.TrimSpace(string(source[receiver.Start:receiver.End])), source[:offset], graph, syntax, offset)
	switch value.Kind {
	case ValueModelClass:
		return Context{Kind: ContextModelMember, Value: value, Identifier: identifier, Replacement: replacement}, true
	case ValueModelInstance:
		return Context{Kind: ContextInstanceMember, Value: value, Identifier: identifier, Replacement: replacement}, true
	default:
		return Context{}, false
	}
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
	if method != "filter" && method != "exclude" && method != "get" {
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
	for start > 0 && isIdentifierByte(source[start-1]) {
		start--
	}
	end := offset
	for end < len(source) && isIdentifierByte(source[end]) {
		end++
	}
	return ByteRange{Start: start, End: end}
}

func isIdentifierByte(value byte) bool {
	return value == '_' || value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9'
}

func inferExpression(expression string, sourcePrefix []byte, graph *schema.Graph, syntax []SyntaxStatement, offset int) Value {
	imports, values := buildEnvironment(sourcePrefix, graph, syntax, offset)
	return resolveExpression(expression, imports, values, graph)
}

func buildEnvironment(source []byte, graph *schema.Graph, syntax []SyntaxStatement, offset int) (map[string]string, map[string]Value) {
	imports := make(map[string]string)
	values := make(map[string]Value)
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
			if statement.ScopeMarker || statement.Guarded {
				continue
			}
			inScope := statement.ScopeKind == "" || statement.ScopeStart == currentScope.ScopeStart && statement.ScopeEnd == currentScope.ScopeEnd && statement.ScopeKind == currentScope.ScopeKind
			if statement.End <= offset && inScope {
				lines = append(lines, []byte(statement.Text))
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
			if match[3] != "" {
				if value := resolveExpression(match[3], imports, values, graph); value.Kind != ValueUnknown {
					values[match[1]] = value
					continue
				}
			}
			annotation := match[2]
			kind := ValueModelInstance
			if open := strings.IndexByte(annotation, '['); open >= 0 && strings.HasSuffix(annotation, "]") {
				container := annotation[:open]
				container = expandImport(container, imports)
				if container != "QuerySet" && !strings.HasSuffix(container, ".QuerySet") {
					continue
				}
				annotation = annotation[open+1 : len(annotation)-1]
				kind = ValueQuerySet
			}
			if label, ok := resolveClass(annotation, imports, graph); ok {
				values[match[1]] = Value{CanonicalLabel: label, Kind: kind}
			}
			continue
		}
		if match := assignmentPattern.FindStringSubmatch(line); match != nil {
			values[match[1]] = resolveExpression(match[2], imports, values, graph)
		}
	}
	return imports, values
}

func resolveExpression(expression string, imports map[string]string, values map[string]Value, graph *schema.Graph) Value {
	expression = strings.TrimSpace(expression)
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
	for position < len(expression) {
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
		position = skipExpressionSpace(expression, position)
		if position < len(expression) && expression[position] == '(' {
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
			if _, ok := graph.Manager(value.CanonicalLabel, name); !ok {
				return Value{}
			}
			value.Kind = ValueManager
		case ValueManager, ValueQuerySet:
			if !called {
				return Value{}
			}
			switch name {
			case "get", "first", "last", "create":
				value.Kind = ValueModelInstance
			case "all", "filter", "exclude", "order_by":
				value.Kind = ValueQuerySet
			default:
				return Value{}
			}
		default:
			return Value{}
		}
	}
	return value
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
