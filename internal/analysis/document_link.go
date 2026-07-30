package analysis

import (
	"regexp"
	"sort"
	"strings"

	"github.com/amirhasanzadehpy/Pogo/internal/schema"
)

type RelationStringReference struct {
	Range       ByteRange
	TargetModel string
}

var classNamePattern = regexp.MustCompile(`^\s*class\s+([A-Za-z_][A-Za-z0-9_]*)\b`)

func ResolveRelationStringReferences(snapshot Snapshot, filePath string, graph *schema.Graph) []RelationStringReference {
	if graph == nil || !snapshot.Parsed || filePath == "" {
		return nil
	}
	var references []RelationStringReference
	for _, call := range snapshot.Calls {
		if !isRelationConstructor(snapshot, graph, call) {
			continue
		}
		field, ok := relationDeclaration(snapshot, filePath, graph, call)
		if !ok {
			continue
		}
		sourceRange, ok := field.SourceRange()
		if !ok || !schema.MatchesSourceDigest(snapshot.Source, sourceRange.SourceDigest) {
			continue
		}
		target, ok := field.RuntimeRelatedModel()
		if !ok {
			target, ok = field.RelatedModel()
		}
		if !ok {
			continue
		}
		arguments := splitStaticArguments(snapshot.Source, call.Arguments)
		for index, argument := range arguments {
			keyword, value := staticKeywordArgument(snapshot.Source, argument)
			if keyword == "" && index == 0 || keyword == "to" {
				if range_, static := diagnosticStringPath(snapshot.Source, value); static {
					references = append(references, RelationStringReference{Range: range_, TargetModel: target})
				}
			}
			if keyword != "related_name" {
				continue
			}
			range_, static := diagnosticStringPath(snapshot.Source, value)
			if !static || range_.Start == range_.End || strings.HasSuffix(string(snapshot.Source[range_.Start:range_.End]), "+") {
				continue
			}
			name := string(snapshot.Source[range_.Start:range_.End])
			access, exists := graph.InstanceAccess(target, name)
			if exists && sameFieldOrigin(access.Field, field) {
				references = append(references, RelationStringReference{Range: range_, TargetModel: target})
			}
		}
	}
	sort.SliceStable(references, func(left, right int) bool {
		if references[left].Range.Start != references[right].Range.Start {
			return references[left].Range.Start < references[right].Range.Start
		}
		if references[left].Range.End != references[right].Range.End {
			return references[left].Range.End < references[right].Range.End
		}
		return references[left].TargetModel < references[right].TargetModel
	})
	return references
}

func isRelationConstructor(snapshot Snapshot, graph *schema.Graph, call SyntaxCall) bool {
	if call.Range.Start < 0 || call.Range.Start > len(snapshot.Source) {
		return false
	}
	expression := call.Method
	if call.Receiver.Start < call.Receiver.End && call.Receiver.Start >= 0 && call.Receiver.End <= len(snapshot.Source) {
		expression = string(snapshot.Source[call.Receiver.Start:call.Receiver.End]) + "." + call.Method
	}
	imports, _ := buildEnvironment(snapshot.Source[:call.Range.Start], graph, snapshot.Syntax, call.Range.Start)
	expression = expandImport(expression, imports)
	switch expression {
	case "django.db.models.ForeignKey", "django.db.models.OneToOneField", "django.db.models.ManyToManyField":
		return true
	default:
		return false
	}
}

func relationDeclaration(snapshot Snapshot, filePath string, graph *schema.Graph, call SyntaxCall) (*schema.FieldRef, bool) {
	className := ""
	classSize := len(snapshot.Source) + 1
	for _, statement := range snapshot.Syntax {
		if !statement.ScopeMarker || statement.ScopeKind != "class_definition" || statement.ScopeStart > call.Range.Start || call.Range.End > statement.ScopeEnd {
			continue
		}
		if size := statement.ScopeEnd - statement.ScopeStart; size < classSize && statement.ScopeStart >= 0 && statement.ScopeEnd <= len(snapshot.Source) {
			match := classNamePattern.FindSubmatch(snapshot.Source[statement.ScopeStart:statement.ScopeEnd])
			if match != nil {
				className = string(match[1])
				classSize = size
			}
		}
	}
	if className == "" {
		return nil, false
	}
	for _, statement := range snapshot.Syntax {
		if statement.ScopeMarker || statement.ScopeKind != "class_definition" || statement.ScopeStart > call.Range.Start || call.Range.End > statement.ScopeEnd || statement.Start > call.Range.Start || call.Range.End > statement.End {
			continue
		}
		equals := topLevelEquals(snapshot.Source, ByteRange{Start: statement.Start, End: call.Range.Start})
		if equals < 0 {
			continue
		}
		value := trimByteRange(snapshot.Source, ByteRange{Start: equals + 1, End: statement.End})
		if value != call.Range {
			continue
		}
		name := trimByteRange(snapshot.Source, ByteRange{Start: statement.Start, End: equals})
		if !identifierText(string(snapshot.Source[name.Start:name.End])) {
			continue
		}
		start := sourceBytePosition(snapshot.Source, statement.Start)
		end := sourceBytePosition(snapshot.Source, statement.End)
		field, exists := graph.RelationFieldForSource(filePath, className, string(snapshot.Source[name.Start:name.End]), start, end)
		if exists {
			return field, true
		}
	}
	return nil, false
}

func sourceBytePosition(source []byte, offset int) schema.Position {
	position := schema.Position{Line: 1}
	for index := 0; index < offset && index < len(source); index++ {
		if source[index] == '\n' {
			position.Line++
			position.Column = 0
		} else {
			position.Column++
		}
	}
	return position
}

func staticKeywordArgument(source []byte, argument ByteRange) (string, ByteRange) {
	equals := topLevelEquals(source, argument)
	if equals < 0 {
		return "", argument
	}
	name := trimByteRange(source, ByteRange{Start: argument.Start, End: equals})
	value := trimByteRange(source, ByteRange{Start: equals + 1, End: argument.End})
	if !identifierText(string(source[name.Start:name.End])) {
		return "", ByteRange{}
	}
	return string(source[name.Start:name.End]), value
}

func sameFieldOrigin(left, right *schema.FieldRef) bool {
	if left == nil || right == nil || left.SourceModel() != right.SourceModel() {
		return false
	}
	leftRange, leftOK := left.SourceRange()
	rightRange, rightOK := right.SourceRange()
	return leftOK && rightOK && leftRange == rightRange
}
