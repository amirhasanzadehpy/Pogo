package analysis

import (
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/amirhasanzadehpy/Pogo/internal/schema"
)

const (
	MaxPathBytes    = 4096
	MaxPathSegments = 64
)

type PathMode uint8

const (
	PathLookup PathMode = iota
	PathProjection
	PathFields
	PathSelectRelated
	PathPrefetchRelated
)

type PathSegment struct {
	Text  string
	Range ByteRange
}

type PathContext struct {
	Method         string
	Mode           PathMode
	Segments       []PathSegment
	ActiveSegment  int
	Replacement    ByteRange
	Arguments      ByteRange
	ActiveArgument int
	OnSeparator    bool
}

type SignatureContext struct {
	Value           Value
	Method          *schema.MethodRef
	ActiveArgument  int
	PositionalIndex int
	Keyword         string
}

func AnalyzeSignatureSyntax(source []byte, offset int, graph *schema.Graph, syntax []SyntaxStatement) (SignatureContext, bool) {
	call, ok := enclosingCall(source, offset)
	if !ok {
		return SignatureContext{}, false
	}
	value := inferExpression(strings.TrimSpace(string(source[call.receiver.Start:call.receiver.End])), source[:offset], graph, syntax, offset)
	method, ok := ResolveMethod(graph, value, call.method)
	if !ok {
		return SignatureContext{}, false
	}
	argument := activeArgument(source, call.argumentsStart, offset)
	end := activeArgumentEnd(source, argument.start)
	keyword, _ := argumentKeyword(source[argument.start:end])
	return SignatureContext{
		Value: value, Method: method, ActiveArgument: argument.index,
		PositionalIndex: positionalArgumentIndex(source, call.argumentsStart, argument.start), Keyword: keyword,
	}, true
}

type PathCandidateKind uint8

const (
	PathCandidateField PathCandidateKind = iota
	PathCandidateTransform
	PathCandidateLookup
)

type PathCandidate struct {
	Name  string
	Kind  PathCandidateKind
	Field *schema.FieldRef
	Rank  int
}

type ResolvedPathSegment struct {
	PathSegment
	Kind  PathCandidateKind
	Field *schema.FieldRef
}

type PathIssueCode string

const (
	IssueUnknownPathSegment   PathIssueCode = "django-orm.unknown-path-segment"
	IssueNonRelationTraversal PathIssueCode = "django-orm.nonrelation-traversal"
	IssueInvalidLookup        PathIssueCode = "django-orm.invalid-lookup-or-transform"
	IssueInvalidProjection    PathIssueCode = "django-orm.invalid-projection-path"
	IssueInvalidSelectRelated PathIssueCode = "django-orm.invalid-select-related-target"
)

type PathIssue struct {
	Code    PathIssueCode
	Segment PathSegment
	Model   string
	Field   *schema.FieldRef
}

func analyzePathContext(source []byte, offset int, graph *schema.Graph, syntax []SyntaxStatement, filePath string) (Context, bool) {
	call, ok := enclosingCall(source, offset)
	if !ok {
		if context, qPath := analyzeQPathContext(source, offset, graph, syntax, filePath); qPath {
			return context, true
		}
		if context, constructorPath := analyzeConstructorPathContext(source, offset, graph, syntax, filePath); constructorPath {
			return context, true
		}
		return analyzeExpressionPathContext(source, offset, graph, syntax, filePath)
	}
	if context, fieldListPath := analyzeFieldListArgumentContext(source, offset, graph, syntax, filePath, call); fieldListPath {
		return context, true
	}
	mode, stringPath, ok := pathMethod(call.method)
	if !ok {
		return Context{}, false
	}
	value := inferExpressionAtPath(strings.TrimSpace(string(source[call.receiver.Start:call.receiver.End])), source[:offset], graph, syntax, offset, filePath)
	if value.Kind != ValueManager && value.Kind != ValueQuerySet {
		return Context{}, false
	}
	if method, overridden := ResolveMethod(graph, value, call.method); overridden && method.OwnerClass() != "django.db.models.query.QuerySet" {
		return Context{}, false
	}
	argument := activeArgument(source, call.argumentsStart, offset)
	var pathRange ByteRange
	if stringPath {
		pathRange, ok = stringPathRange(source, argument, offset)
	} else {
		pathRange, ok = keywordPathRange(source, argument, offset)
	}
	if call.method == "order_by" && pathRange.Start < pathRange.End && (source[pathRange.Start] == '-' || source[pathRange.Start] == '+') {
		pathRange.Start++
	}
	if !ok || pathRange.End-pathRange.Start > MaxPathBytes {
		return Context{}, false
	}
	segments, active, onSeparator, ok := splitPath(source, pathRange, offset)
	if !ok || len(segments) > MaxPathSegments {
		return Context{}, false
	}
	path := &PathContext{
		Method: call.method, Mode: mode, Segments: segments, ActiveSegment: active,
		Replacement: segments[active].Range,
		Arguments:   ByteRange{Start: call.argumentsStart, End: offset}, ActiveArgument: argument.index,
		OnSeparator: onSeparator,
	}
	if !stringPath && len(segments) == 1 {
		return Context{Kind: ContextQueryKeyword, Value: value, Identifier: segments[0].Text, Replacement: path.Replacement}, true
	}
	return Context{Kind: ContextORMPath, Value: value, Identifier: segments[active].Text, Replacement: path.Replacement, Path: path}, true
}

func analyzeExpressionPathContext(source []byte, offset int, graph *schema.Graph, syntax []SyntaxStatement, filePath string) (Context, bool) {
	expression, ok := enclosingFunctionCall(source, offset)
	if !ok {
		return Context{}, false
	}
	imports, _ := buildEnvironmentAtPath(source[:offset], graph, syntax, offset, filePath)
	if !isAnnotationExpressionFunction(expandImport(expression.name, imports)) {
		return Context{}, false
	}
	call, ok := enclosingCallBefore(source, offset, expression.opening)
	if !ok || call.method != "annotate" && call.method != "alias" && call.method != "aggregate" {
		return Context{}, false
	}
	value := inferExpressionAtPath(strings.TrimSpace(string(source[call.receiver.Start:call.receiver.End])), source[:offset], graph, syntax, offset, filePath)
	if value.Kind != ValueManager && value.Kind != ValueQuerySet {
		return Context{}, false
	}
	argument := activeArgument(source, expression.argumentsStart, offset)
	pathRange, ok := stringPathRange(source, argument, offset)
	if !ok || pathRange.End-pathRange.Start > MaxPathBytes {
		return Context{}, false
	}
	segments, active, onSeparator, ok := splitPath(source, pathRange, offset)
	if !ok || len(segments) > MaxPathSegments {
		return Context{}, false
	}
	path := &PathContext{
		Method: call.method, Mode: PathProjection, Segments: segments, ActiveSegment: active,
		Replacement: segments[active].Range, Arguments: ByteRange{Start: expression.argumentsStart, End: offset}, ActiveArgument: argument.index,
		OnSeparator: onSeparator,
	}
	return Context{Kind: ContextORMPath, Value: value, Identifier: segments[active].Text, Replacement: path.Replacement, Path: path}, true
}

func analyzeQPathContext(source []byte, offset int, graph *schema.Graph, syntax []SyntaxStatement, filePath string) (Context, bool) {
	qCall, ok := enclosingFunctionCall(source, offset)
	if !ok {
		return Context{}, false
	}
	imports, _ := buildEnvironmentAtPath(source[:offset], graph, syntax, offset, filePath)
	if expandImport(qCall.name, imports) != "django.db.models.Q" {
		return Context{}, false
	}
	call, ok := enclosingCallBefore(source, offset, qCall.opening)
	if !ok {
		return Context{}, false
	}
	mode, stringPath, ok := pathMethod(call.method)
	if !ok || stringPath {
		return Context{}, false
	}
	value := inferExpressionAtPath(strings.TrimSpace(string(source[call.receiver.Start:call.receiver.End])), source[:offset], graph, syntax, offset, filePath)
	if value.Kind != ValueManager && value.Kind != ValueQuerySet {
		return Context{}, false
	}
	if method, overridden := ResolveMethod(graph, value, call.method); overridden && method.OwnerClass() != "django.db.models.query.QuerySet" {
		return Context{}, false
	}
	argument := activeArgument(source, qCall.argumentsStart, offset)
	pathRange, ok := keywordPathRange(source, argument, offset)
	if !ok || pathRange.End-pathRange.Start > MaxPathBytes {
		return Context{}, false
	}
	segments, active, onSeparator, ok := splitPath(source, pathRange, offset)
	if !ok || len(segments) > MaxPathSegments {
		return Context{}, false
	}
	path := &PathContext{
		Method: call.method, Mode: mode, Segments: segments, ActiveSegment: active,
		Replacement: segments[active].Range, Arguments: ByteRange{Start: qCall.argumentsStart, End: offset}, ActiveArgument: argument.index,
		OnSeparator: onSeparator,
	}
	if len(segments) == 1 {
		return Context{Kind: ContextQueryKeyword, Value: value, Identifier: segments[0].Text, Replacement: path.Replacement}, true
	}
	return Context{Kind: ContextORMPath, Value: value, Identifier: segments[active].Text, Replacement: path.Replacement, Path: path}, true
}

// analyzeConstructorPathContext recognizes keyword arguments passed to a bare model
// constructor call, e.g. Book(title=..., author=...), and treats them like the field
// keywords already supported for Model.objects.create(...).
func analyzeConstructorPathContext(source []byte, offset int, graph *schema.Graph, syntax []SyntaxStatement, filePath string) (Context, bool) {
	call, ok := enclosingFunctionCall(source, offset)
	if !ok {
		return Context{}, false
	}
	imports, _ := buildEnvironmentAtPath(source[:offset], graph, syntax, offset, filePath)
	label, ok := resolveClass(call.name, imports, graph)
	if !ok {
		return Context{}, false
	}
	value := Value{CanonicalLabel: label, Kind: ValueModelClass}
	argument := activeArgument(source, call.argumentsStart, offset)
	pathRange, ok := keywordPathRange(source, argument, offset)
	if !ok || pathRange.End-pathRange.Start > MaxPathBytes {
		return Context{}, false
	}
	segments, active, onSeparator, ok := splitPath(source, pathRange, offset)
	if !ok || len(segments) > MaxPathSegments {
		return Context{}, false
	}
	path := &PathContext{
		Method: call.name, Mode: PathLookup, Segments: segments, ActiveSegment: active,
		Replacement: segments[active].Range, Arguments: ByteRange{Start: call.argumentsStart, End: offset}, ActiveArgument: argument.index,
		OnSeparator: onSeparator,
	}
	if len(segments) == 1 {
		return Context{Kind: ContextQueryKeyword, Value: value, Identifier: segments[0].Text, Replacement: path.Replacement}, true
	}
	return Context{Kind: ContextORMPath, Value: value, Identifier: segments[active].Text, Replacement: path.Replacement, Path: path}, true
}

// analyzeFieldListArgumentContext recognizes the field-name-list keyword arguments of
// bulk_create/bulk_update (unique_fields, update_fields, fields), whose values are a
// list literal of field name strings rather than a single positional string.
func analyzeFieldListArgumentContext(source []byte, offset int, graph *schema.Graph, syntax []SyntaxStatement, filePath string, call callContext) (Context, bool) {
	argument := activeArgument(source, call.argumentsStart, offset)
	argumentEnd := activeArgumentEnd(source, argument.start)
	keyword, isKeyword := argumentKeyword(source[argument.start:argumentEnd])
	if !isKeyword || !isFieldListKeyword(call.method, keyword) {
		return Context{}, false
	}
	valueStart, ok := keywordArgumentValueStart(source, argument.start, argumentEnd)
	if !ok {
		return Context{}, false
	}
	value := inferExpressionAtPath(strings.TrimSpace(string(source[call.receiver.Start:call.receiver.End])), source[:offset], graph, syntax, offset, filePath)
	if value.Kind != ValueManager && value.Kind != ValueQuerySet {
		return Context{}, false
	}
	if method, overridden := ResolveMethod(graph, value, call.method); overridden && method.OwnerClass() != "django.db.models.query.QuerySet" {
		return Context{}, false
	}
	pathRange, ok := stringListPathRange(source, valueStart, offset)
	if !ok || pathRange.End-pathRange.Start > MaxPathBytes {
		return Context{}, false
	}
	segments, active, onSeparator, ok := splitPath(source, pathRange, offset)
	if !ok || len(segments) > MaxPathSegments {
		return Context{}, false
	}
	path := &PathContext{
		Method: call.method, Mode: PathFields, Segments: segments, ActiveSegment: active,
		Replacement: segments[active].Range, Arguments: ByteRange{Start: call.argumentsStart, End: offset}, ActiveArgument: argument.index,
		OnSeparator: onSeparator,
	}
	return Context{Kind: ContextORMPath, Value: value, Identifier: segments[active].Text, Replacement: path.Replacement, Path: path}, true
}

func isFieldListKeyword(method, keyword string) bool {
	switch method {
	case "bulk_create":
		return keyword == "unique_fields" || keyword == "update_fields"
	case "bulk_update":
		return keyword == "fields"
	default:
		return false
	}
}

// keywordArgumentValueStart returns the offset of the value following "keyword=" within
// source[start:end], skipping leading whitespace. It mirrors argumentKeyword's depth-aware
// scan for the top-level '=' so nested parens/brackets/quotes in the keyword don't confuse it.
func keywordArgumentValueStart(source []byte, start, end int) (int, bool) {
	depth := 0
	var quote byte
	escaped := false
	for index := start; index < end; index++ {
		value := source[index]
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
			if depth == 0 {
				valueStart := index + 1
				for valueStart < end && isSpace(source[valueStart]) {
					valueStart++
				}
				return valueStart, true
			}
		}
	}
	return 0, false
}

type callContext struct {
	method         string
	receiver       ByteRange
	argumentsStart int
}

type functionCallContext struct {
	name           string
	opening        int
	argumentsStart int
}

func enclosingCall(source []byte, offset int) (callContext, bool) {
	if offset < 0 || offset > len(source) {
		return callContext{}, false
	}
	code := pythonCodeMask(source, offset)
	stack := make([]int, 0, 8)
	for index := 0; index < offset; index++ {
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
		return callContext{}, false
	}
	return callAfterOpening(source, stack[len(stack)-1])
}

func enclosingCallBefore(source []byte, offset, opening int) (callContext, bool) {
	if offset < 0 || offset > len(source) {
		return callContext{}, false
	}
	code := pythonCodeMask(source, offset)
	stack := make([]int, 0, 8)
	for index := 0; index < offset; index++ {
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
	for index := len(stack) - 1; index >= 0; index-- {
		if stack[index] >= opening {
			continue
		}
		if call, ok := callAfterOpening(source, stack[index]); ok {
			return call, true
		}
	}
	return callContext{}, false
}

func callAfterOpening(source []byte, opening int) (callContext, bool) {
	code := pythonCodeMask(source, opening)
	nameEnd := opening
	for nameEnd > 0 && isSpace(source[nameEnd-1]) {
		nameEnd--
	}
	nameStart := nameEnd
	for nameStart > 0 && isIdentifierByte(source[nameStart-1]) && code[nameStart-1] {
		nameStart--
	}
	if nameStart == nameEnd || nameStart == 0 || source[nameStart-1] != '.' {
		return callContext{}, false
	}
	receiver := expressionBefore(source, nameStart-1)
	if receiver.Start == receiver.End {
		return callContext{}, false
	}
	return callContext{method: string(source[nameStart:nameEnd]), receiver: receiver, argumentsStart: opening + 1}, true
}

func enclosingFunctionCall(source []byte, offset int) (functionCallContext, bool) {
	if offset < 0 || offset > len(source) {
		return functionCallContext{}, false
	}
	code := pythonCodeMask(source, offset)
	stack := make([]int, 0, 8)
	for index := 0; index < offset; index++ {
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
		return functionCallContext{}, false
	}
	opening := stack[len(stack)-1]
	nameEnd := opening
	for nameEnd > 0 && isSpace(source[nameEnd-1]) {
		nameEnd--
	}
	nameStart := nameEnd
	for nameStart > 0 && isIdentifierByte(source[nameStart-1]) && code[nameStart-1] {
		nameStart--
	}
	if nameStart == nameEnd || nameStart > 0 && source[nameStart-1] == '.' {
		return functionCallContext{}, false
	}
	return functionCallContext{name: string(source[nameStart:nameEnd]), opening: opening, argumentsStart: opening + 1}, true
}

type argumentContext struct {
	start int
	index int
}

func activeArgument(source []byte, start, offset int) argumentContext {
	argument := argumentContext{start: start}
	depth := 0
	var quote byte
	triple := false
	escaped := false
	comment := false
	for index := start; index < offset; index++ {
		value := source[index]
		if comment {
			if value == '\n' {
				comment = false
			}
			continue
		}
		if quote != 0 {
			if triple {
				if value == quote && index+2 < offset && source[index+1] == quote && source[index+2] == quote {
					quote, triple = 0, false
					index += 2
				}
				continue
			}
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
			if index+2 < offset && source[index+1] == value && source[index+2] == value {
				triple = true
				index += 2
			}
		case '#':
			comment = true
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				argument.start = index + 1
				argument.index++
			}
		}
	}
	return argument
}

func positionalArgumentIndex(source []byte, start, activeStart int) int {
	position := 0
	segmentStart := start
	depth := 0
	var quote byte
	escaped := false
	comment := false
	for index := start; index < activeStart; index++ {
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
		case '#':
			comment = true
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				if _, keyword := argumentKeyword(source[segmentStart:index]); !keyword {
					position++
				}
				segmentStart = index + 1
			}
		}
	}
	return position
}

func activeArgumentEnd(source []byte, start int) int {
	depth := 0
	var quote byte
	escaped := false
	comment := false
	for index := start; index < len(source); index++ {
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
		case '#':
			comment = true
		case '(', '[', '{':
			depth++
		case ')':
			if depth == 0 {
				return index
			}
			depth--
		case ']', '}':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				return index
			}
		}
	}
	return len(source)
}

func argumentKeyword(argument []byte) (string, bool) {
	depth := 0
	var quote byte
	escaped := false
	for index, value := range argument {
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
			quote = byte(value)
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			if depth > 0 {
				depth--
			}
		case '=':
			if depth == 0 {
				name := strings.TrimSpace(string(argument[:index]))
				if name != "" && identifierText(name) {
					return name, true
				}
				return "", false
			}
		}
	}
	return "", false
}

func identifierText(value string) bool {
	if value == "" {
		return false
	}
	for _, rune_ := range value {
		if !isIdentifierRune(rune_) {
			return false
		}
	}
	return true
}

func keywordPathRange(source []byte, argument argumentContext, offset int) (ByteRange, bool) {
	start := argument.start
	for start < offset && isSpace(source[start]) {
		start++
	}
	if start > offset {
		return ByteRange{}, false
	}
	if start < offset && !identifierText(string(source[start:offset])) {
		return ByteRange{}, false
	}
	end := offset
	for end < len(source) {
		rune_, size := utf8.DecodeRune(source[end:])
		if rune_ == utf8.RuneError && size == 1 || !isIdentifierRune(rune_) {
			break
		}
		end += size
	}
	if end < len(source) {
		index := end
		for index < len(source) && isSpace(source[index]) {
			index++
		}
		if index < len(source) && source[index] != '=' && source[index] != ',' && source[index] != ')' {
			return ByteRange{}, false
		}
	}
	return ByteRange{Start: start, End: end}, true
}

func stringPathRange(source []byte, argument argumentContext, offset int) (ByteRange, bool) {
	start := argument.start
	for start < offset && isSpace(source[start]) {
		start++
	}
	prefixStart := start
	for start < offset && isIdentifierByte(source[start]) {
		start++
	}
	prefix := strings.ToLower(string(source[prefixStart:start]))
	if strings.ContainsAny(prefix, "bf") || (prefix != "" && prefix != "r" && prefix != "u") {
		return ByteRange{}, false
	}
	if start >= len(source) || (source[start] != '\'' && source[start] != '"') {
		return ByteRange{}, false
	}
	quote := source[start]
	quoteWidth := 1
	if start+2 < len(source) && source[start+1] == quote && source[start+2] == quote {
		quoteWidth = 3
	}
	contentStart := start + quoteWidth
	if offset < contentStart {
		return ByteRange{}, false
	}
	end := contentStart
	escaped := false
	for end < len(source) {
		if source[end] == '\\' {
			escaped = !escaped
			end++
			continue
		}
		if source[end] == quote && !escaped {
			if quoteWidth == 1 || end+2 < len(source) && source[end+1] == quote && source[end+2] == quote {
				break
			}
		}
		escaped = false
		if source[end] == '\n' && quoteWidth == 1 {
			break
		}
		end++
	}
	if offset > end || strings.ContainsRune(string(source[contentStart:end]), '\\') {
		return ByteRange{}, false
	}
	return ByteRange{Start: contentStart, End: end}, true
}

// stringListPathRange locates the quoted string element containing offset inside a
// list literal, e.g. the "b" in ["a", "b", "c"]. start must point at the list's
// opening '['. It returns false if offset falls outside any element's string content,
// or if an element isn't a simple (optionally r/u prefixed) quoted string.
func stringListPathRange(source []byte, start, offset int) (ByteRange, bool) {
	elements, ok := stringListElements(source, start)
	if !ok {
		return ByteRange{}, false
	}
	for _, element := range elements {
		if offset >= element.Start && offset <= element.End {
			return element, true
		}
	}
	return ByteRange{}, false
}

// stringListElements returns the byte ranges of every quoted string element in a
// list literal, e.g. ["a", "b", "c"]. start must point at the list's opening '['.
// It returns false if the list is unterminated or an element isn't a simple
// (optionally r/u prefixed) quoted string.
func stringListElements(source []byte, start int) ([]ByteRange, bool) {
	for start < len(source) && isSpace(source[start]) {
		start++
	}
	if start >= len(source) || source[start] != '[' {
		return nil, false
	}
	var elements []ByteRange
	index := start + 1
	for index < len(source) {
		for index < len(source) && isSpace(source[index]) {
			index++
		}
		if index >= len(source) {
			return nil, false
		}
		if source[index] == ']' {
			return elements, true
		}
		prefixStart := index
		for index < len(source) && isIdentifierByte(source[index]) {
			index++
		}
		prefix := strings.ToLower(string(source[prefixStart:index]))
		if index >= len(source) || (source[index] != '\'' && source[index] != '"') {
			return nil, false
		}
		if strings.ContainsAny(prefix, "bf") || (prefix != "" && prefix != "r" && prefix != "u") {
			return nil, false
		}
		quote := source[index]
		quoteWidth := 1
		if index+2 < len(source) && source[index+1] == quote && source[index+2] == quote {
			quoteWidth = 3
		}
		contentStart := index + quoteWidth
		contentEnd := contentStart
		escaped := false
		for contentEnd < len(source) {
			if source[contentEnd] == '\\' {
				escaped = !escaped
				contentEnd++
				continue
			}
			if source[contentEnd] == quote && !escaped {
				if quoteWidth == 1 || contentEnd+2 < len(source) && source[contentEnd+1] == quote && source[contentEnd+2] == quote {
					break
				}
			}
			escaped = false
			if source[contentEnd] == '\n' && quoteWidth == 1 {
				return nil, false
			}
			contentEnd++
		}
		if contentEnd >= len(source) || strings.ContainsRune(string(source[contentStart:contentEnd]), '\\') {
			return nil, false
		}
		elements = append(elements, ByteRange{Start: contentStart, End: contentEnd})
		index = contentEnd + quoteWidth
		for index < len(source) && isSpace(source[index]) {
			index++
		}
		if index < len(source) && source[index] == ',' {
			index++
			continue
		}
	}
	return nil, false
}

func splitPath(source []byte, path ByteRange, offset int) ([]PathSegment, int, bool, bool) {
	if offset < path.Start || offset > path.End {
		return nil, 0, false, false
	}
	segments := make([]PathSegment, 0, 8)
	start := path.Start
	active := -1
	onSeparator := false
	for index := path.Start; index <= path.End; index++ {
		separator := index+1 < path.End && source[index] == '_' && source[index+1] == '_'
		if index == path.End || separator {
			if separator && (offset == index || offset == index+1) {
				onSeparator = true
			}
			segment := PathSegment{Text: string(source[start:index]), Range: ByteRange{Start: start, End: index}}
			segments = append(segments, segment)
			if offset >= start && offset <= index {
				active = len(segments) - 1
			}
			if separator {
				if offset == index+1 {
					active = len(segments)
				}
				index++
				start = index + 1
			}
		}
	}
	if active == len(segments) {
		segments = append(segments, PathSegment{Range: ByteRange{Start: offset, End: offset}})
	}
	if len(segments) == 0 || active < 0 {
		return nil, 0, false, false
	}
	return segments, active, onSeparator, true
}

func pathMethod(method string) (PathMode, bool, bool) {
	switch method {
	case "filter", "exclude", "get", "get_or_create", "update_or_create", "create", "update":
		return PathLookup, false, true
	case "values", "values_list", "order_by", "earliest", "latest", "dates", "datetimes":
		return PathProjection, true, true
	case "only", "defer":
		return PathFields, true, true
	case "select_related":
		return PathSelectRelated, true, true
	case "prefetch_related":
		return PathPrefetchRelated, true, true
	default:
		return 0, false, false
	}
}

// isAnnotationExpressionFunction reports whether a qualified callee refers to a Django ORM
// expression whose positional string arguments name a field or annotation alias on the current
// queryset (F(...), the aggregate functions, or anything under django.db.models.functions).
func isAnnotationExpressionFunction(qualifiedName string) bool {
	if strings.HasPrefix(qualifiedName, "django.db.models.functions.") {
		return true
	}
	switch qualifiedName {
	case "django.db.models.F", "django.db.models.Count", "django.db.models.Sum", "django.db.models.Avg",
		"django.db.models.Min", "django.db.models.Max", "django.db.models.StdDev", "django.db.models.Variance":
		return true
	default:
		return false
	}
}

func isAnnotationAlias(annotations []string, name string) bool {
	if name == "" {
		return false
	}
	for _, alias := range annotations {
		if alias == name {
			return true
		}
	}
	return false
}

func isSpace(value byte) bool {
	return value == ' ' || value == '\t' || value == '\r' || value == '\n'
}

type resolverState struct {
	model         string
	field         *schema.FieldRef
	relationField *schema.FieldRef
	transforms    []string
}

func CompletePath(graph *schema.Graph, canonicalLabel string, mode PathMode, segments []PathSegment, active int) []PathCandidate {
	if graph == nil || active < 0 || active >= len(segments) {
		return nil
	}
	state := resolverState{model: canonicalLabel}
	for index := 0; index < active; index++ {
		var ok bool
		state, _, ok = consumePathSegment(graph, mode, state, segments[index], true)
		if !ok {
			return nil
		}
	}
	candidates := candidatesForState(graph, mode, state)
	prefix := segments[active].Text
	filtered := candidates[:0]
	for _, candidate := range candidates {
		if strings.HasPrefix(candidate.Name, prefix) {
			filtered = append(filtered, candidate)
		}
	}
	sort.SliceStable(filtered, func(left, right int) bool {
		if filtered[left].Rank != filtered[right].Rank {
			return filtered[left].Rank < filtered[right].Rank
		}
		return filtered[left].Name < filtered[right].Name
	})
	return filtered
}

func ResolvePathSegment(graph *schema.Graph, canonicalLabel string, mode PathMode, segments []PathSegment, target int) (ResolvedPathSegment, bool) {
	if graph == nil || target < 0 || target >= len(segments) {
		return ResolvedPathSegment{}, false
	}
	state := resolverState{model: canonicalLabel}
	var resolved ResolvedPathSegment
	for index := 0; index <= target; index++ {
		var ok bool
		state, resolved, ok = consumePathSegment(graph, mode, state, segments[index], index < len(segments)-1)
		if !ok {
			return ResolvedPathSegment{}, false
		}
	}
	return resolved, true
}

func ValidatePath(graph *schema.Graph, canonicalLabel string, mode PathMode, segments []PathSegment, annotations []string) (PathIssue, bool) {
	if graph == nil || canonicalLabel == "" || len(segments) == 0 || len(segments) > MaxPathSegments {
		return PathIssue{}, false
	}
	if (mode == PathLookup || mode == PathProjection) && isAnnotationAlias(annotations, segments[0].Text) {
		return PathIssue{}, false
	}
	state := resolverState{model: canonicalLabel}
	for index, segment := range segments {
		if segment.Text == "" {
			issue := PathIssue{Segment: segment, Model: state.model, Field: state.field}
			if issue.Field == nil {
				issue.Field = state.relationField
			}
			switch mode {
			case PathLookup:
				if state.field != nil {
					issue.Code = IssueInvalidLookup
				} else {
					issue.Code = IssueUnknownPathSegment
				}
			case PathProjection:
				if state.field != nil {
					issue.Code = IssueInvalidProjection
				} else {
					issue.Code = IssueUnknownPathSegment
				}
			case PathFields:
				if state.field != nil {
					issue.Code = IssueNonRelationTraversal
				} else {
					issue.Code = IssueUnknownPathSegment
				}
			default:
				issue.Code = IssueUnknownPathSegment
			}
			return issue, true
		}
		previous := state
		next, _, ok := consumePathSegment(graph, mode, state, segment, index < len(segments)-1)
		if ok {
			state = next
			continue
		}
		issue := PathIssue{Segment: segment, Model: previous.model, Field: previous.field}
		if issue.Field == nil {
			issue.Field = previous.relationField
		}
		if issue.Field != nil && pathMetadataIndeterminate(issue.Field, previous.transforms, segment.Text, index < len(segments)-1) {
			return PathIssue{}, false
		}
		switch mode {
		case PathSelectRelated:
			if _, exists := graph.QueryAccess(previous.model, segment.Text); exists {
				issue.Code = IssueInvalidSelectRelated
			} else {
				issue.Code = IssueUnknownPathSegment
			}
		case PathPrefetchRelated:
			if access, exists := graph.QueryAccess(previous.model, segment.Text); exists && !access.Field.IsRelation() {
				issue.Code = IssueNonRelationTraversal
			} else if access, exists := graph.InstanceAccess(previous.model, segment.Text); exists && !access.Field.IsRelation() {
				issue.Code = IssueNonRelationTraversal
			} else {
				issue.Code = IssueUnknownPathSegment
			}
		case PathFields:
			if previous.field != nil {
				issue.Code = IssueNonRelationTraversal
			} else {
				issue.Code = IssueUnknownPathSegment
			}
		case PathProjection:
			if previous.field != nil {
				issue.Code = IssueInvalidProjection
			} else if previous.relationField != nil {
				issue.Code = IssueUnknownPathSegment
			} else {
				issue.Code = IssueUnknownPathSegment
			}
		case PathLookup:
			if previous.field != nil {
				issue.Code = IssueInvalidLookup
			} else if previous.relationField != nil && previous.model != "" {
				issue.Code = IssueUnknownPathSegment
			} else {
				issue.Code = IssueUnknownPathSegment
			}
		}
		return issue, true
	}
	return PathIssue{}, false
}

func pathMetadataIndeterminate(field *schema.FieldRef, transforms []string, failed string, hasMore bool) bool {
	if field.LookupPathsTruncated() {
		return true
	}
	for _, lookup := range field.UnsupportedLookups() {
		if lookup == failed && !hasMore {
			return true
		}
	}
	if !field.IsRelation() {
		for _, path := range field.LookupPaths() {
			for _, transform := range path.Transforms {
				if transform == "*" && len(transforms) >= len(path.Transforms) {
					return true
				}
			}
		}
	}
	return false
}

func consumePathSegment(graph *schema.Graph, mode PathMode, state resolverState, segment PathSegment, hasMore bool) (resolverState, ResolvedPathSegment, bool) {
	if segment.Text == "" {
		return state, ResolvedPathSegment{}, false
	}
	if mode == PathSelectRelated || mode == PathPrefetchRelated {
		context := schema.RelationSelectRelated
		if mode == PathPrefetchRelated {
			context = schema.RelationPrefetchRelated
		}
		access, ok := graph.Relation(state.model, segment.Text, context)
		if !ok {
			return state, ResolvedPathSegment{}, false
		}
		resolved := ResolvedPathSegment{PathSegment: segment, Kind: PathCandidateField, Field: access.Field}
		related, relatedOK := access.Field.RelatedModel()
		if hasMore && !relatedOK {
			return state, ResolvedPathSegment{}, false
		}
		return resolverState{model: related}, resolved, true
	}

	if state.model != "" {
		if access, ok := graph.QueryAccess(state.model, segment.Text); ok {
			return stateFromField(access), ResolvedPathSegment{PathSegment: segment, Kind: PathCandidateField, Field: access.Field}, true
		}
		if state.relationField == nil {
			return state, ResolvedPathSegment{}, false
		}
	}
	field := state.field
	if field == nil {
		field = state.relationField
	}
	kind, transforms, ok := consumeFieldSuffix(field, state.transforms, segment.Text, mode, hasMore)
	if !ok {
		return state, ResolvedPathSegment{}, false
	}
	return resolverState{field: field, transforms: transforms}, ResolvedPathSegment{PathSegment: segment, Kind: kind, Field: field}, true
}

func stateFromField(access schema.FieldAccess) resolverState {
	if access.Field.IsRelation() {
		if related, ok := access.Field.RelatedModel(); ok {
			return resolverState{model: related, relationField: access.Field}
		}
	}
	return resolverState{field: access.Field}
}

func candidatesForState(graph *schema.Graph, mode PathMode, state resolverState) []PathCandidate {
	candidates := make([]PathCandidate, 0, 32)
	seen := make(map[string]struct{})
	add := func(candidate PathCandidate) {
		if candidate.Name == "" {
			return
		}
		key := candidate.Name
		if _, exists := seen[key]; exists {
			return
		}
		seen[key] = struct{}{}
		candidates = append(candidates, candidate)
	}
	if mode == PathSelectRelated || mode == PathPrefetchRelated {
		context := schema.RelationSelectRelated
		if mode == PathPrefetchRelated {
			context = schema.RelationPrefetchRelated
		}
		graph.VisitRelations(state.model, context, func(access schema.FieldAccess) bool {
			rank := 0
			if mode == PathPrefetchRelated {
				direction, _ := access.Field.RelationDirection()
				cardinality, _ := access.Field.RelationCardinality()
				if direction == schema.RelationReverse || cardinality == schema.RelationManyToMany || cardinality == schema.RelationOneToMany {
					rank = -1
				}
			}
			add(PathCandidate{Name: access.Name, Kind: PathCandidateField, Field: access.Field, Rank: rank})
			return true
		})
		return candidates
	}
	if state.model != "" {
		graph.VisitQueryFields(state.model, func(access schema.FieldAccess) bool {
			add(PathCandidate{Name: access.Name, Kind: PathCandidateField, Field: access.Field})
			return true
		})
	}
	field := state.field
	if field == nil {
		field = state.relationField
	}
	if field != nil {
		for _, candidate := range fieldSuffixCandidates(field, state.transforms, mode) {
			add(candidate)
		}
	}
	return candidates
}

func fieldSuffixCandidates(field *schema.FieldRef, prefix []string, mode PathMode) []PathCandidate {
	if mode == PathFields {
		return nil
	}
	var candidates []PathCandidate
	seen := make(map[string]struct{})
	field.VisitLookupPaths(func(path schema.LookupPath) bool {
		if len(path.Transforms) < len(prefix) || !equalStrings(path.Transforms[:len(prefix)], prefix) {
			return true
		}
		if len(path.Transforms) > len(prefix) {
			name := path.Transforms[len(prefix)]
			if name != "*" {
				if _, exists := seen[name]; !exists {
					seen[name] = struct{}{}
					candidates = append(candidates, PathCandidate{Name: name, Kind: PathCandidateTransform, Field: field, Rank: 1})
				}
			}
			return true
		}
		if mode == PathLookup {
			for _, lookup := range path.Lookups {
				if field.IsRelation() && !relationLookup(lookup) {
					continue
				}
				if _, exists := seen[lookup]; !exists {
					seen[lookup] = struct{}{}
					candidates = append(candidates, PathCandidate{Name: lookup, Kind: PathCandidateLookup, Field: field, Rank: 2})
				}
			}
		}
		return true
	})
	return candidates
}

func consumeFieldSuffix(field *schema.FieldRef, prefix []string, segment string, mode PathMode, hasMore bool) (PathCandidateKind, []string, bool) {
	if field == nil || mode == PathFields {
		return 0, nil, false
	}
	var wildcard bool
	var lookup bool
	var transform bool
	nextPrefix := prefix
	field.VisitLookupPaths(func(path schema.LookupPath) bool {
		if len(path.Transforms) < len(prefix) || !equalStrings(path.Transforms[:len(prefix)], prefix) {
			return true
		}
		if len(path.Transforms) > len(prefix) {
			next := path.Transforms[len(prefix)]
			if next == segment {
				nextPrefix = append(append([]string(nil), prefix...), segment)
				transform = true
				wildcard = false
				lookup = false
				return false
			}
			if next == "*" {
				wildcard = true
			}
			return true
		}
		if mode == PathLookup && !hasMore {
			for _, candidate := range path.Lookups {
				if candidate == segment && (!field.IsRelation() || relationLookup(candidate)) {
					lookup = true
					return false
				}
			}
		}
		return true
	})
	if transform {
		return PathCandidateTransform, nextPrefix, true
	}
	if lookup {
		return PathCandidateLookup, prefix, true
	}
	if wildcard {
		return PathCandidateTransform, append(append([]string(nil), prefix...), "*"), true
	}
	return 0, nil, false
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func relationLookup(name string) bool {
	switch name {
	case "exact", "gt", "gte", "in", "isnull", "lt", "lte":
		return true
	default:
		return false
	}
}
