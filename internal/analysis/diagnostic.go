package analysis

import (
	"fmt"
	"sort"
	"strings"

	"github.com/amirhasanzadehpy/Pogo/internal/schema"
)

type ORMIssue struct {
	Code    PathIssueCode
	Message string
	Range   ByteRange
}

func DiagnoseORM(snapshot Snapshot, graph *schema.Graph) []ORMIssue {
	issues := make([]ORMIssue, 0)
	if graph == nil || !snapshot.Parsed {
		return issues
	}
	for _, call := range snapshot.Calls {
		mode, stringPaths, supported := pathMethod(call.Method)
		if !supported || call.Receiver.Start < 0 || call.Receiver.End > len(snapshot.Source) || call.Receiver.Start >= call.Receiver.End {
			continue
		}
		value := inferExpression(
			strings.TrimSpace(string(snapshot.Source[call.Receiver.Start:call.Receiver.End])),
			snapshot.Source[:call.Range.Start], graph, snapshot.Syntax, call.Range.Start,
		)
		if value.Kind != ValueManager && value.Kind != ValueQuerySet {
			continue
		}
		if _, overridden := ResolveMethod(graph, value, call.Method); overridden {
			continue
		}
		for _, argument := range splitStaticArguments(snapshot.Source, call.Arguments) {
			var pathRange ByteRange
			var ok bool
			if stringPaths {
				pathRange, ok = diagnosticStringPath(snapshot.Source, argument)
			} else {
				pathRange, ok = diagnosticKeywordPath(snapshot.Source, argument)
			}
			if !ok || pathRange.End-pathRange.Start > MaxPathBytes {
				continue
			}
			segments, ok := diagnosticPathSegments(snapshot.Source, pathRange)
			if !ok || len(segments) > MaxPathSegments {
				continue
			}
			problem, invalid := ValidatePath(graph, value.CanonicalLabel, mode, segments)
			if !invalid {
				continue
			}
			issues = append(issues, ORMIssue{Code: problem.Code, Message: pathIssueMessage(problem), Range: problem.Segment.Range})
		}
	}
	sort.SliceStable(issues, func(left, right int) bool {
		if issues[left].Range.Start != issues[right].Range.Start {
			return issues[left].Range.Start < issues[right].Range.Start
		}
		if issues[left].Range.End != issues[right].Range.End {
			return issues[left].Range.End < issues[right].Range.End
		}
		if issues[left].Code != issues[right].Code {
			return issues[left].Code < issues[right].Code
		}
		return issues[left].Message < issues[right].Message
	})
	return issues
}

func pathIssueMessage(issue PathIssue) string {
	segment := issue.Segment.Text
	switch issue.Code {
	case IssueNonRelationTraversal:
		field := "field"
		if issue.Field != nil {
			field = issue.Field.Name()
		}
		return fmt.Sprintf("Cannot traverse non-relation field %q with segment %q.", field, segment)
	case IssueInvalidLookup:
		field := "field"
		if issue.Field != nil {
			field = issue.Field.Name()
		}
		return fmt.Sprintf("Invalid Django ORM lookup or transform %q for field %q.", segment, field)
	case IssueInvalidProjection:
		field := "field"
		if issue.Field != nil {
			field = issue.Field.Name()
		}
		return fmt.Sprintf("Invalid Django ORM projection segment %q after field %q.", segment, field)
	case IssueInvalidSelectRelated:
		return fmt.Sprintf("select_related() target %q is not a single-valued relation on %q.", segment, issue.Model)
	default:
		return fmt.Sprintf("Unknown Django ORM field or relation %q on %q.", segment, issue.Model)
	}
}

func splitStaticArguments(source []byte, arguments ByteRange) []ByteRange {
	if arguments.Start < 0 || arguments.End > len(source) || arguments.End-arguments.Start < 2 || source[arguments.Start] != '(' || source[arguments.End-1] != ')' {
		return nil
	}
	start := arguments.Start + 1
	end := arguments.End - 1
	segmentStart := start
	depth := 0
	var quote byte
	triple := false
	escaped := false
	comment := false
	leadingComment := false
	var ranges []ByteRange
	appendRange := func(segmentEnd int) {
		range_ := trimByteRange(source, ByteRange{Start: segmentStart, End: segmentEnd})
		if range_.Start < range_.End {
			ranges = append(ranges, range_)
		}
	}
	for index := start; index < end; index++ {
		value := source[index]
		if comment {
			if value == '\n' {
				comment = false
				if leadingComment {
					segmentStart = index + 1
					leadingComment = false
				}
			}
			continue
		}
		if quote != 0 {
			if triple {
				if value == quote && index+2 < end && source[index+1] == quote && source[index+2] == quote {
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
			if index+2 < end && source[index+1] == value && source[index+2] == value {
				triple = true
				index += 2
			}
		case '#':
			leadingComment = trimByteRange(source, ByteRange{Start: segmentStart, End: index}).Start == index
			comment = true
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				appendRange(index)
				segmentStart = index + 1
			}
		}
	}
	appendRange(end)
	return ranges
}

func diagnosticKeywordPath(source []byte, argument ByteRange) (ByteRange, bool) {
	equals := topLevelEquals(source, argument)
	if equals < 0 {
		return ByteRange{}, false
	}
	name := trimByteRange(source, ByteRange{Start: argument.Start, End: equals})
	value := trimByteRange(source, ByteRange{Start: equals + 1, End: argument.End})
	if name.Start == name.End || value.Start == value.End || !identifierText(string(source[name.Start:name.End])) {
		return ByteRange{}, false
	}
	return name, true
}

func topLevelEquals(source []byte, range_ ByteRange) int {
	depth := 0
	var quote byte
	escaped := false
	for index := range_.Start; index < range_.End; index++ {
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
				return index
			}
		}
	}
	return -1
}

func diagnosticStringPath(source []byte, argument ByteRange) (ByteRange, bool) {
	start := argument.Start
	for start < argument.End && isIdentifierByte(source[start]) {
		start++
	}
	prefix := strings.ToLower(string(source[argument.Start:start]))
	if strings.ContainsAny(prefix, "bf") || (prefix != "" && prefix != "r" && prefix != "u") || start >= argument.End {
		return ByteRange{}, false
	}
	quote := source[start]
	if quote != '\'' && quote != '"' {
		return ByteRange{}, false
	}
	width := 1
	if start+2 < argument.End && source[start+1] == quote && source[start+2] == quote {
		width = 3
	}
	contentStart := start + width
	contentEnd := argument.End - width
	if contentEnd < contentStart {
		return ByteRange{}, false
	}
	for index := 0; index < width; index++ {
		if source[argument.End-width+index] != quote {
			return ByteRange{}, false
		}
	}
	if strings.ContainsRune(string(source[contentStart:contentEnd]), '\\') || strings.ContainsRune(string(source[contentStart:contentEnd]), rune(quote)) {
		return ByteRange{}, false
	}
	return ByteRange{Start: contentStart, End: contentEnd}, true
}

func diagnosticPathSegments(source []byte, path ByteRange) ([]PathSegment, bool) {
	segments := make([]PathSegment, 0, 8)
	start := path.Start
	for index := path.Start; index <= path.End; index++ {
		if index != path.End && !(index+1 < path.End && source[index] == '_' && source[index+1] == '_') {
			continue
		}
		if start == index {
			range_ := ByteRange{Start: index, End: index}
			if index < path.End {
				range_.End = index + 2
			} else if index-path.Start >= 2 {
				range_.Start = index - 2
			}
			segments = append(segments, PathSegment{Range: range_})
			if index < path.End {
				index++
				start = index + 1
			}
			continue
		}
		segments = append(segments, PathSegment{Text: string(source[start:index]), Range: ByteRange{Start: start, End: index}})
		if index < path.End {
			index++
			start = index + 1
		}
	}
	return segments, len(segments) > 0
}

func trimByteRange(source []byte, range_ ByteRange) ByteRange {
	for range_.Start < range_.End && isSpace(source[range_.Start]) {
		range_.Start++
	}
	for range_.End > range_.Start && isSpace(source[range_.End-1]) {
		range_.End--
	}
	return range_
}
