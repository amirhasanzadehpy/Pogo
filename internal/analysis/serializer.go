package analysis

import (
	"sort"
	"strings"

	"github.com/amirhasanzadehpy/Pogo/internal/schema"
)

// SerializerField describes a schema-backed or explicitly declared API field.
// Declaration is an in-document byte range; it takes precedence over model source.
type SerializerField struct {
	Name        string
	Field       *schema.FieldRef
	Declaration ByteRange
	Source      string
}

const maxSerializerTokens = 4096
const maxSerializerExpressionBytes = 64 * 1024

type serializerToken struct {
	text    string
	span    ByteRange
	literal bool
}

func serializerBindingName(text string) string {
	text = strings.TrimSpace(text)
	end := 0
	for end < len(text) && isIdentifierByte(text[end]) {
		end++
	}
	if end == 0 || !identifierText(text[:end]) {
		return ""
	}
	rest := strings.TrimSpace(text[end:])
	if strings.HasPrefix(rest, "=") || strings.HasPrefix(rest, ":") {
		return text[:end]
	}
	return ""
}

// Tokenize only bounded, literal configuration expressions. Escaped, prefixed,
// triple-quoted and incomplete strings are omitted rather than mis-ranged.
func serializerTokens(source []byte, span ByteRange) ([]serializerToken, bool) {
	if span.Start < 0 || span.End > len(source) || span.End < span.Start || span.End-span.Start > maxSerializerExpressionBytes {
		return nil, false
	}
	var tokens []serializerToken
	for i := span.Start; i < span.End; {
		if len(tokens) == maxSerializerTokens {
			return nil, false
		}
		c := source[i]
		if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
			i++
			continue
		}
		if c == '#' {
			for i < span.End && source[i] != '\n' {
				i++
			}
			continue
		}
		start := i
		if c == '\'' || c == '"' {
			if i+2 < span.End && source[i+1] == c && source[i+2] == c {
				return nil, false
			}
			i++
			for i < span.End && source[i] != c {
				if source[i] == '\\' || source[i] == '\n' || source[i] == '\r' {
					return nil, false
				}
				i++
			}
			if i == span.End {
				return nil, false
			}
			tokens = append(tokens, serializerToken{text: string(source[start+1 : i]), span: ByteRange{start + 1, i}, literal: true})
			i++
			continue
		}
		if isIdentifierByte(c) {
			for i < span.End && isIdentifierByte(source[i]) {
				i++
			}
		} else {
			i++
		}
		tokens = append(tokens, serializerToken{text: string(source[start:i]), span: ByteRange{start, i}})
	}
	return tokens, true
}

func serializerAssignment(source []byte, statement SyntaxStatement) (string, []serializerToken, bool) {
	tokens, ok := serializerTokens(source, ByteRange{statement.Start, statement.End})
	if !ok || len(tokens) < 3 || !identifierText(tokens[0].text) || tokens[0].literal || tokens[1].text != "=" {
		return "", nil, false
	}
	return tokens[0].text, tokens[2:], true
}

func serializerReference(tokens []serializerToken) (string, bool) {
	var name strings.Builder
	for i, token := range tokens {
		if token.literal || i%2 == 0 && !identifierText(token.text) || i%2 == 1 && token.text != "." {
			return "", false
		}
		name.WriteString(token.text)
	}
	return name.String(), len(tokens) > 0 && len(tokens)%2 == 1
}

func serializerBase(source []byte, class SyntaxStatement, imports map[string]string) bool {
	// The first colon ends the supported plain class header. Custom bases and
	// multiple inheritance require an authority we deliberately do not invent.
	end := class.Start
	for end < class.End && end < len(source) && source[end] != ':' {
		end++
	}
	tokens, ok := serializerTokens(source, ByteRange{class.Start, end})
	if !ok || len(tokens) < 5 || tokens[0].text != "class" || tokens[2].text != "(" || tokens[len(tokens)-1].text != ")" {
		return false
	}
	base := tokens[3 : len(tokens)-1]
	if len(base) > 0 && base[len(base)-1].text == "," {
		base = base[:len(base)-1]
	}
	name, ok := serializerReference(base)
	if !ok {
		return false
	}
	name = expandImport(name, imports)
	return name == "rest_framework.serializers.ModelSerializer" || name == "rest_framework.serializers.HyperlinkedModelSerializer"
}

func serializerStringList(tokens []serializerToken) ([]serializerToken, bool) {
	if len(tokens) < 2 {
		return nil, false
	}
	close := "]"
	if tokens[0].text == "(" {
		close = ")"
	} else if tokens[0].text != "[" {
		return nil, false
	}
	if tokens[len(tokens)-1].text != close {
		return nil, false
	}
	var values []serializerToken
	for i := 1; i < len(tokens)-1; i++ {
		if !tokens[i].literal {
			return nil, false
		}
		values = append(values, tokens[i])
		i++
		if i < len(tokens)-1 && tokens[i].text != "," {
			return nil, false
		}
	}
	if close == ")" && len(values) == 1 && len(tokens) == 3 {
		return nil, false
	}
	return values, true
}

func serializerCall(tokens []serializerToken, imports map[string]string) ([]serializerToken, bool) {
	open := 0
	for open < len(tokens) && tokens[open].text != "(" {
		open++
	}
	if open == len(tokens) || tokens[len(tokens)-1].text != ")" {
		return nil, false
	}
	name, ok := serializerReference(tokens[:open])
	if !ok {
		return nil, false
	}
	name = expandImport(name, imports)
	const prefix = "rest_framework.serializers."
	if !strings.HasPrefix(name, prefix) || strings.Contains(name[len(prefix):], ".") || !strings.HasSuffix(name, "Field") {
		return nil, false
	}
	switch name[len(prefix):] {
	case "Field", "BooleanField", "CharField", "EmailField", "RegexField", "SlugField", "URLField", "UUIDField", "FilePathField", "IPAddressField",
		"IntegerField", "FloatField", "DecimalField", "DateTimeField", "DateField", "TimeField", "DurationField", "ChoiceField", "MultipleChoiceField",
		"FileField", "ImageField", "ListField", "DictField", "HStoreField", "JSONField", "ReadOnlyField", "HiddenField", "ModelField", "SerializerMethodField",
		"RelatedField", "PrimaryKeyRelatedField", "SlugRelatedField", "StringRelatedField", "HyperlinkedRelatedField", "HyperlinkedIdentityField":
	default:
		return nil, false
	}
	// Require one complete call, rather than a call combined with another
	// expression. Brackets inside strings do not participate in nesting.
	var stack []string
	for i := open; i < len(tokens); i++ {
		if tokens[i].literal {
			continue
		}
		switch tokens[i].text {
		case "(", "[", "{":
			stack = append(stack, tokens[i].text)
		case ")", "]", "}":
			if len(stack) == 0 {
				return nil, false
			}
			opening := stack[len(stack)-1]
			if opening == "(" && tokens[i].text != ")" || opening == "[" && tokens[i].text != "]" || opening == "{" && tokens[i].text != "}" {
				return nil, false
			}
			stack = stack[:len(stack)-1]
			if len(stack) == 0 && i != len(tokens)-1 {
				return nil, false
			}
		}
	}
	if len(stack) != 0 {
		return nil, false
	}
	return tokens[open+1 : len(tokens)-1], true
}

func serializerSource(tokens []serializerToken) (serializerToken, bool) {
	depth := 0
	for i, token := range tokens {
		if depth == 0 && !token.literal && token.text == "source" && (i == 0 || tokens[i-1].text == ",") && i+2 < len(tokens) && tokens[i+1].text == "=" {
			if tokens[i+2].literal && (i+3 == len(tokens) || tokens[i+3].text == ",") {
				return tokens[i+2], true
			}
			return serializerToken{}, true
		}
		if !token.literal {
			switch token.text {
			case "(", "[", "{":
				depth++
			case ")", "]", "}":
				depth--
			}
		}
	}
	return serializerToken{}, false
}

func serializerAttribute(graph *schema.Graph, label, path string) (*schema.FieldRef, bool) {
	if len(path) > MaxPathBytes {
		return nil, false
	}
	segments := strings.Split(path, ".")
	if len(segments) > MaxPathSegments {
		return nil, false
	}
	for i, segment := range segments {
		access, ok := graph.InstanceAccess(label, segment)
		if !ok || access.Field == nil {
			return nil, false
		}
		if i == len(segments)-1 {
			return access.Field, true
		}
		if access.Kind == schema.FieldAccessAttname {
			return nil, false
		}
		label, ok = singleRelatedInstance(access.Field)
		if !ok {
			return nil, false
		}
	}
	return nil, false
}

func analyzeSerializerContext(source []byte, offset int, graph *schema.Graph, syntax []SyntaxStatement) (Context, bool) {
	// Ordinary identifier/ORM completion pays only a scope scan, no tokenization.
	var scope SyntaxStatement
	for _, s := range syntax {
		if s.ScopeMarker && s.Start <= offset && offset <= s.End && (scope.End == 0 || s.End-s.Start < scope.End-scope.Start) {
			scope = s
		}
	}
	if scope.ScopeKind != "class_definition" {
		return Context{}, false
	}
	class := scope
	meta := SyntaxStatement{}
	if definitionName(scope.Text) == "Meta" {
		meta = scope
		class = SyntaxStatement{}
		for _, s := range syntax {
			if s.ScopeMarker && s.ScopeKind == "class_definition" && s.Start < meta.Start && s.End >= meta.End && (class.End == 0 || s.End-s.Start < class.End-class.Start) {
				class = s
			}
		}
	}
	if class.End == 0 {
		return Context{}, false
	}
	imports, _ := buildEnvironment(source[:class.Start], graph, syntax, class.Start)
	if !serializerBase(source, class, imports) {
		return Context{}, false
	}
	for _, s := range syntax {
		if s.Guarded && s.Start <= class.Start && s.End >= class.End || s.ScopeMarker && s.ScopeKind == "function_definition" && s.Start < class.Start && s.End >= class.End {
			return Context{}, false
		}
		if s.ScopeMarker && s.ScopeKind == "class_definition" && definitionName(s.Text) == "Meta" && s.Start > class.Start && s.End <= class.End {
			parent := serializerParentScope(s, syntax)
			if parent.Start != class.Start {
				continue
			}
			if meta.End != 0 && meta.Start != s.Start {
				return Context{}, false
			}
			meta = s
		}
	}
	if meta.End == 0 {
		return Context{}, false
	}
	// Class-local bindings cannot be resolved using the module import map.
	// Remove them even when their values are dynamic or their syntax unsupported.
	for _, s := range syntax {
		if !s.ScopeMarker && (s.ScopeStart == class.Start || s.ScopeStart == meta.Start) {
			if name := serializerBindingName(s.Text); name != "" {
				delete(imports, name)
			} else {
				for _, name := range statementBindingNames(s.Text) {
					delete(imports, name)
				}
			}
		}
	}
	label := ""
	var active []serializerToken
	option := ""
	var included []serializerToken
	includeAll := false
	hasFields := false
	var excluded []serializerToken
	hasExclude := false
	for _, s := range syntax {
		if s.ScopeMarker || s.ScopeStart != meta.Start || s.ScopeEnd != meta.End {
			continue
		}
		name, tokens, ok := serializerAssignment(source, s)
		if !ok {
			continue
		}
		if name == "model" {
			if label != "" || s.Guarded {
				return Context{}, false
			}
			reference, valid := serializerReference(tokens)
			if !valid {
				return Context{}, false
			}
			label, valid = resolveClass(reference, imports, graph)
			if !valid {
				return Context{}, false
			}
		}
		if name == "fields" {
			hasFields = true
			included, _ = serializerStringList(tokens)
			includeAll = len(tokens) == 1 && tokens[0].literal && tokens[0].text == "__all__"
			if s.Guarded {
				included = nil
				includeAll = false
			}
		}
		if name == "exclude" && !s.Guarded {
			excluded, hasExclude = serializerStringList(tokens)
		}
		if s.Start <= offset && offset <= s.End && !s.Guarded {
			option, active = name, tokens
		}
	}
	if label == "" {
		return Context{}, false
	}
	var declarations []SerializerField
	blocked := make(map[string]bool)
	for _, s := range syntax {
		if s.ScopeMarker || s.ScopeStart != class.Start || s.ScopeEnd != class.End {
			continue
		}
		name, tokens, ok := serializerAssignment(source, s)
		if !ok {
			if name := serializerBindingName(s.Text); name != "" {
				blocked[name] = true
			} else {
				for _, name := range statementBindingNames(s.Text) {
					blocked[name] = true
				}
			}
			continue
		}
		if blocked[name] {
			return Context{}, false
		}
		blocked[name] = true
		args, valid := serializerCall(tokens, imports)
		if !valid || s.Guarded {
			continue
		}
		field := SerializerField{Name: name, Declaration: ByteRange{s.Start, s.Start + len(name)}}
		if token, hasSource := serializerSource(args); hasSource {
			if token.literal {
				field.Source = token.text
				field.Field, _ = serializerAttribute(graph, label, token.text)
			}
		}
		declarations = append(declarations, field)
		if scope.Start == class.Start && s.Start <= offset && offset <= s.End {
			if token, hasSource := serializerSource(args); hasSource && token.literal && token.span.Start <= offset && offset <= token.span.End {
				return serializerSourceContext(source, offset, graph, label, token)
			}
		}
	}
	if scope.Start != meta.Start {
		return Context{}, false
	}
	var replacement ByteRange
	found := false
	switch option {
	case "fields", "exclude", "read_only_fields":
		values, ok := serializerStringList(active)
		if !ok {
			return Context{}, false
		}
		for _, token := range values {
			if token.span.Start <= offset && offset <= token.span.End {
				replacement, found = token.span, true
				break
			}
		}
	case "extra_kwargs":
		if len(active) < 2 || active[0].text != "{" || active[len(active)-1].text != "}" {
			return Context{}, false
		}
		depth := 0
		for i, token := range active {
			if token.literal && depth == 1 && i > 0 && (active[i-1].text == "{" || active[i-1].text == ",") && i+1 < len(active) && active[i+1].text == ":" && token.span.Start <= offset && offset <= token.span.End {
				replacement, found = token.span, true
				break
			}
			if !token.literal {
				switch token.text {
				case "{", "[", "(":
					depth++
				case "}", "]", ")":
					depth--
				}
			}
		}
	default:
		return Context{}, false
	}
	if !found {
		return Context{}, false
	}
	var fields []SerializerField
	graph.VisitInstanceFields(label, func(access schema.FieldAccess) bool {
		if access.Field != nil && access.Kind != schema.FieldAccessAttname && (access.Kind == schema.FieldAccessReverse || access.Name == access.Field.Name()) && !blocked[access.Name] {
			fields = append(fields, SerializerField{Name: access.Name, Field: access.Field})
		}
		return true
	})
	if option == "fields" || option == "read_only_fields" {
		fields = append(fields, declarations...)
	}
	if option == "read_only_fields" || option == "extra_kwargs" {
		filtered := fields[:0]
		for _, field := range fields {
			selected := includeAll || !hasFields && hasExclude
			if !hasFields && hasExclude {
				for _, token := range excluded {
					if token.text == field.Name {
						selected = false
						break
					}
				}
			}
			for _, token := range included {
				if token.text == field.Name {
					selected = true
					break
				}
			}
			if selected {
				filtered = append(filtered, field)
			}
		}
		fields = filtered
	}
	sort.Slice(fields, func(i, j int) bool { return fields[i].Name < fields[j].Name })
	return Context{Kind: ContextSerializerField, Value: Value{CanonicalLabel: label, Kind: ValueModelClass}, Identifier: string(source[replacement.Start:replacement.End]), Replacement: replacement, SerializerFields: fields}, true
}

func serializerParentScope(child SyntaxStatement, syntax []SyntaxStatement) SyntaxStatement {
	var parent SyntaxStatement
	for _, candidate := range syntax {
		if candidate.ScopeMarker && candidate.Start < child.Start && candidate.End >= child.End && (parent.End == 0 || candidate.End-candidate.Start < parent.End-parent.Start) {
			parent = candidate
		}
	}
	return parent
}

func serializerSourceContext(source []byte, offset int, graph *schema.Graph, label string, token serializerToken) (Context, bool) {
	if token.text == "*" || len(token.text) > MaxPathBytes {
		return Context{}, false
	}
	start := token.span.Start
	segments := 0
	for i := token.span.Start; i < offset; i++ {
		if source[i] != '.' {
			continue
		}
		segments++
		if segments >= MaxPathSegments {
			return Context{}, false
		}
		access, ok := graph.InstanceAccess(label, string(source[start:i]))
		if !ok || access.Kind == schema.FieldAccessAttname {
			return Context{}, false
		}
		label, ok = singleRelatedInstance(access.Field)
		if !ok {
			return Context{}, false
		}
		start = i + 1
	}
	end := start
	for end < token.span.End && source[end] != '.' {
		end++
	}
	var fields []SerializerField
	graph.VisitInstanceFields(label, func(access schema.FieldAccess) bool {
		fields = append(fields, SerializerField{Name: access.Name, Field: access.Field})
		return true
	})
	return Context{Kind: ContextSerializerField, Value: Value{CanonicalLabel: label, Kind: ValueModelInstance}, Identifier: string(source[start:end]), Replacement: ByteRange{start, end}, SerializerFields: fields}, true
}

func (context Context) SerializerField() (SerializerField, bool) {
	for _, field := range context.SerializerFields {
		if field.Name == context.Identifier {
			return field, true
		}
	}
	return SerializerField{}, false
}

// ResolveSerializerDeclaration returns a same-document field declaration rather
// than a persisted schema source range. Unsaved serializer edits remain navigable.
func ResolveSerializerDeclaration(snapshot Snapshot, offset int, graph *schema.Graph) (ByteRange, bool) {
	if graph == nil || offset < 0 || offset > len(snapshot.Source) {
		return ByteRange{}, false
	}
	context, ok := analyzeSerializerContext(snapshot.Source, offset, graph, snapshot.Syntax)
	if !ok {
		return ByteRange{}, false
	}
	field, ok := context.SerializerField()
	return field.Declaration, ok && field.Declaration.End != 0
}
