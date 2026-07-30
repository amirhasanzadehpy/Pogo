package lsp

import (
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/amirhasanzadehpy/Pogo/internal/analysis"
	"github.com/amirhasanzadehpy/Pogo/internal/schema"
	"github.com/tliron/glsp"
	protocol "github.com/tliron/glsp/protocol_3_16"
)

const maxNavigationSourceSize = 16 * 1024 * 1024

func (features *Features) definition(_ *glsp.Context, params *protocol.DefinitionParams) (any, error) {
	if features == nil || params == nil {
		return nil, nil
	}
	return features.Definition(string(params.TextDocument.URI), analysisPosition(params.Position))
}

func (features *Features) documentLink(_ *glsp.Context, params *protocol.DocumentLinkParams) ([]protocol.DocumentLink, error) {
	if features == nil || params == nil {
		return []protocol.DocumentLink{}, nil
	}
	return features.DocumentLinks(string(params.TextDocument.URI))
}

func (features *Features) Definition(uri string, position analysis.Position) (*protocol.Location, error) {
	snapshot, ok := features.documents.Snapshot(uri)
	if !ok {
		return nil, nil
	}
	offset, ok := analysis.ByteOffset(snapshot.Source, position)
	if !ok {
		return nil, errors.New("definition position is not valid for the document")
	}
	graph, _ := features.cache.Load()
	sourceRange, ok := analysis.ResolveDefinitionSyntax(snapshot.Source, offset, graph, snapshot.Syntax)
	if !ok {
		return nil, nil
	}
	location, ok := features.sourceLocation(sourceRange)
	if !ok {
		return nil, nil
	}
	return &location, nil
}

func (features *Features) DocumentLinks(uri string) ([]protocol.DocumentLink, error) {
	links := make([]protocol.DocumentLink, 0)
	snapshot, ok := features.documents.Snapshot(uri)
	if !ok {
		return links, nil
	}
	graph, _ := features.cache.Load()
	if graph == nil {
		return links, nil
	}

	appendLink := func(byteRange analysis.ByteRange, target schema.SourceRange) {
		range_, valid := protocolRange(snapshot.Source, byteRange)
		if !valid {
			return
		}
		location, valid := features.sourceLocation(target)
		if !valid {
			return
		}
		targetURI := location.URI
		links = append(links, protocol.DocumentLink{Range: range_, Target: &targetURI})
	}
	for _, reference := range analysis.ResolveStaticORMPathReferences(snapshot, graph) {
		if sourceRange, exists := reference.Field.SourceRange(); exists {
			appendLink(reference.Segment.Range, sourceRange)
		}
	}
	if filePath, exists := localFilePath(uri); exists {
		for _, reference := range analysis.ResolveRelationStringReferences(snapshot, filePath, graph) {
			if sourceRange, found := graph.ModelSourceRange(reference.TargetModel); found {
				appendLink(reference.Range, sourceRange)
			}
		}
	}
	sort.SliceStable(links, func(left, right int) bool {
		if links[left].Range.Start.Line != links[right].Range.Start.Line {
			return links[left].Range.Start.Line < links[right].Range.Start.Line
		}
		if links[left].Range.Start.Character != links[right].Range.Start.Character {
			return links[left].Range.Start.Character < links[right].Range.Start.Character
		}
		if links[left].Range.End.Line != links[right].Range.End.Line {
			return links[left].Range.End.Line < links[right].Range.End.Line
		}
		if links[left].Range.End.Character != links[right].Range.End.Character {
			return links[left].Range.End.Character < links[right].Range.End.Character
		}
		return string(*links[left].Target) < string(*links[right].Target)
	})
	if len(links) > 1 {
		unique := links[:1]
		for _, link := range links[1:] {
			previous := unique[len(unique)-1]
			if previous.Range != link.Range || *previous.Target != *link.Target {
				unique = append(unique, link)
			}
		}
		links = unique
	}
	return links, nil
}

func (features *Features) sourceLocation(sourceRange schema.SourceRange) (protocol.Location, bool) {
	location, ok := sourceLocation(sourceRange)
	if !ok || features == nil {
		return location, ok
	}
	targetInfo, targetErr := os.Stat(sourceRange.FilePath)
	for _, snapshot := range features.documents.Snapshots() {
		openPath, valid := localFilePath(snapshot.URI)
		if !valid {
			continue
		}
		openInfo, err := os.Stat(openPath)
		sameFile := targetErr == nil && err == nil && os.SameFile(targetInfo, openInfo)
		if !sameFile && filepath.Clean(openPath) != filepath.Clean(sourceRange.FilePath) {
			continue
		}
		if !schema.MatchesSourceDigest(snapshot.Source, sourceRange.SourceDigest) {
			return protocol.Location{}, false
		}
	}
	return location, true
}

func sourceLocation(sourceRange schema.SourceRange) (protocol.Location, bool) {
	if sourceRange.FilePath == "" || strings.IndexByte(sourceRange.FilePath, 0) >= 0 || !filepath.IsAbs(sourceRange.FilePath) {
		return protocol.Location{}, false
	}
	info, err := os.Stat(sourceRange.FilePath)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxNavigationSourceSize {
		return protocol.Location{}, false
	}
	source, err := os.ReadFile(sourceRange.FilePath)
	if err != nil {
		return protocol.Location{}, false
	}
	if !schema.MatchesSourceDigest(source, sourceRange.SourceDigest) {
		return protocol.Location{}, false
	}
	if len(source) >= 3 && string(source[:3]) == "\xef\xbb\xbf" {
		source = source[3:]
	}
	if !utf8.Valid(source) {
		return protocol.Location{}, false
	}
	start, ok := sourcePosition(source, sourceRange.Start)
	if !ok {
		return protocol.Location{}, false
	}
	end, ok := sourcePosition(source, sourceRange.End)
	if !ok || end.Line < start.Line || end.Line == start.Line && end.Character < start.Character {
		return protocol.Location{}, false
	}
	uri, ok := sourceFileURI(sourceRange.FilePath)
	if !ok {
		return protocol.Location{}, false
	}
	return protocol.Location{URI: uri, Range: protocol.Range{Start: start, End: end}}, true
}

func sourcePosition(source []byte, position schema.Position) (protocol.Position, bool) {
	if position.Line < 1 || position.Column < 0 {
		return protocol.Position{}, false
	}
	line := 1
	start := 0
	for index := 0; index < len(source) && line < position.Line; index++ {
		if source[index] == '\n' {
			line++
			start = index + 1
		} else if source[index] == '\r' && (index+1 == len(source) || source[index+1] != '\n') {
			line++
			start = index + 1
		}
	}
	if line != position.Line {
		return protocol.Position{}, false
	}
	end := start
	for end < len(source) && source[end] != '\n' && source[end] != '\r' {
		end++
	}
	column := position.Column
	if column > end-start || !utf8.Valid(source[start:start+column]) {
		return protocol.Position{}, false
	}
	character := 0
	for _, rune_ := range string(source[start : start+column]) {
		character += len(utf16.Encode([]rune{rune_}))
	}
	return protocol.Position{Line: protocol.UInteger(position.Line - 1), Character: protocol.UInteger(character)}, true
}

func sourceFileURI(path string) (protocol.DocumentUri, bool) {
	if path == "" || strings.IndexByte(path, 0) >= 0 || !utf8.ValidString(path) || !filepath.IsAbs(path) {
		return "", false
	}
	cleaned := filepath.Clean(path)
	if runtime.GOOS == "windows" {
		slashed := filepath.ToSlash(cleaned)
		if strings.HasPrefix(slashed, "//?/") || strings.HasPrefix(slashed, "//./") {
			return "", false
		}
		if strings.HasPrefix(slashed, "//") {
			parts := strings.SplitN(strings.TrimPrefix(slashed, "//"), "/", 2)
			if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
				return "", false
			}
			return protocol.DocumentUri((&url.URL{Scheme: "file", Host: parts[0], Path: "/" + parts[1]}).String()), true
		}
		cleaned = "/" + slashed
	}
	return protocol.DocumentUri((&url.URL{Scheme: "file", Path: cleaned}).String()), true
}
