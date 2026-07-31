package lsp

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/amirhasanzadehpy/Pogo/internal/analysis"
	"github.com/amirhasanzadehpy/Pogo/internal/schema"
	protocol "github.com/tliron/glsp/protocol_3_16"
)

const navigationModelsSource = `from django.db import models
class Author(models.Model):
    name = models.CharField()
class Book(models.Model):
    author = models.ForeignKey("Author", on_delete=models.CASCADE, related_name="books")
    title = models.CharField()
`

func TestSourceFileURIRoundTripsNativePath(t *testing.T) {
	want := filepath.Join(t.TempDir(), "model with space.py")
	uri := mustSourceFileURI(t, want)
	got, ok := localFilePath(uri)
	if !ok || got != filepath.Clean(want) {
		t.Fatalf("localFilePath(%q) = %q, %v; want %q", uri, got, ok, want)
	}
	if got, ok := localFilePath("file://"); ok || got != "" {
		t.Fatalf("localFilePath(file://) = %q, %v", got, ok)
	}
}

func TestDefinitionReturnsExactSchemaLocations(t *testing.T) {
	features, modelsPath := navigationTestFeatures(t)
	defer features.Close()
	uri := "file:///workspace/queries.py"
	tests := []struct {
		name   string
		source string
		want   protocol.Range
	}{
		{"model alias", "from myapp.models import Book as Novel\nNo|vel.objects.all()", protocol.Range{Start: protocol.Position{Line: 3}, End: protocol.Position{Line: 5, Character: 30}}},
		{"manager", "from myapp.models import Book\nBook.ob|jects.all()", protocol.Range{Start: protocol.Position{Line: 3}, End: protocol.Position{Line: 5, Character: 30}}},
		{"forward relation", "from myapp.models import Book\nBook.objects.filter(au|thor__name=1)", protocol.Range{Start: protocol.Position{Line: 4, Character: 4}, End: protocol.Position{Line: 4, Character: 88}}},
		{"related field", "from myapp.models import Book\nBook.objects.filter(author__na|me=1)", protocol.Range{Start: protocol.Position{Line: 2, Character: 4}, End: protocol.Position{Line: 2, Character: 29}}},
		{"terminal lookup", "from myapp.models import Book\nBook.objects.filter(title__ico|ntains=1)", protocol.Range{Start: protocol.Position{Line: 5, Character: 4}, End: protocol.Position{Line: 5, Character: 30}}},
		{"string path", "from myapp.models import Book\nBook.objects.values(\"author__na|me\")", protocol.Range{Start: protocol.Position{Line: 2, Character: 4}, End: protocol.Position{Line: 2, Character: 29}}},
		{"reverse accessor", "from myapp.models import Author\nauthor = Author()\nauthor.bo|oks", protocol.Range{Start: protocol.Position{Line: 4, Character: 4}, End: protocol.Position{Line: 4, Character: 88}}},
		{"queryset method", "from myapp.models import Book\nBook.objects.act|ive()", protocol.Range{Start: protocol.Position{Line: 5, Character: 4}, End: protocol.Position{Line: 5, Character: 30}}},
		{"manager method", "from myapp.models import Book\nBook.catalog.fea|tured()", protocol.Range{Start: protocol.Position{Line: 5, Character: 4}, End: protocol.Position{Line: 5, Character: 30}}},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source, position := lspSourceAtCursor(t, test.source)
			if err := features.documents.Open(uri, int32(index+1), string(source)); err != nil {
				t.Fatal(err)
			}
			location, err := features.Definition(uri, position)
			if err != nil || location == nil {
				t.Fatalf("Definition() = %#v, %v", location, err)
			}
			wantURI := protocol.DocumentUri(mustSourceFileURI(t, modelsPath))
			if location.URI != wantURI || location.Range != test.want {
				t.Fatalf("Definition() = %#v, want URI %s range %#v", location, wantURI, test.want)
			}
			features.documents.Close(uri)
		})
	}
}

func TestDefinitionSoftFailsForMissingAndInvalidTargets(t *testing.T) {
	features, modelsPath := navigationTestFeatures(t)
	defer features.Close()
	uri := "file:///workspace/missing.py"
	source, position := lspSourceAtCursor(t, "from myapp.models import Book\nBook.ti|tle")
	if err := features.documents.Open(uri, 1, string(source)); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(modelsPath); err != nil {
		t.Fatal(err)
	}
	location, err := features.Definition(uri, position)
	if err != nil || location != nil {
		t.Fatalf("Definition() = %#v, %v", location, err)
	}
	if _, err := features.Definition(uri, analysis.Position{Line: 99}); err == nil {
		t.Fatal("invalid request position did not return an error")
	}
}

func TestNavigationRejectsStaleSchemaSource(t *testing.T) {
	features, modelsPath := navigationTestFeatures(t)
	defer features.Close()
	uri := "file:///workspace/stale.py"
	source, position := lspSourceAtCursor(t, "from myapp.models import Book\nBook.ti|tle")
	if err := features.documents.Open(uri, 1, string(source)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(modelsPath, []byte("# changed\n"+navigationModelsSource), 0o644); err != nil {
		t.Fatal(err)
	}
	location, err := features.Definition(uri, position)
	if err != nil || location != nil {
		t.Fatalf("stale Definition() = %#v, %v", location, err)
	}
}

func TestDefinitionRejectsUnsavedTargetChanges(t *testing.T) {
	features, modelsPath := navigationTestFeatures(t)
	defer features.Close()
	modelsURL, err := url.Parse(mustSourceFileURI(t, modelsPath))
	if err != nil {
		t.Fatal(err)
	}
	modelsURL.Host = "localhost"
	modelsURI := modelsURL.String()
	if err := features.documents.Open(modelsURI, 1, "# unsaved\n"+navigationModelsSource); err != nil {
		t.Fatal(err)
	}
	queryURI := "file:///workspace/unsaved-target.py"
	source, position := lspSourceAtCursor(t, "from myapp.models import Book\nBook.ti|tle")
	if err := features.documents.Open(queryURI, 1, string(source)); err != nil {
		t.Fatal(err)
	}
	location, err := features.Definition(queryURI, position)
	if err != nil || location != nil {
		t.Fatalf("unsaved-target Definition() = %#v, %v", location, err)
	}
}

func TestDocumentLinksResolveQueryPathsAndRelationStrings(t *testing.T) {
	features, modelsPath := navigationTestFeatures(t)
	defer features.Close()

	queryURI := "file:///workspace/links.py"
	query := "from myapp.models import Book\nBook.objects.values(\"author__name\", \"title__lower\")\n"
	if err := features.documents.Open(queryURI, 1, query); err != nil {
		t.Fatal(err)
	}
	links, err := features.DocumentLinks(queryURI)
	if err != nil || len(links) != 4 {
		t.Fatalf("DocumentLinks() = %#v, %v", links, err)
	}
	wantRanges := []protocol.Range{
		{Start: protocol.Position{Line: 1, Character: 21}, End: protocol.Position{Line: 1, Character: 27}},
		{Start: protocol.Position{Line: 1, Character: 29}, End: protocol.Position{Line: 1, Character: 33}},
		{Start: protocol.Position{Line: 1, Character: 37}, End: protocol.Position{Line: 1, Character: 42}},
		{Start: protocol.Position{Line: 1, Character: 44}, End: protocol.Position{Line: 1, Character: 49}},
	}
	wantURI := protocol.DocumentUri(mustSourceFileURI(t, modelsPath))
	for index, link := range links {
		if link.Range != wantRanges[index] || link.Target == nil || *link.Target != wantURI {
			t.Errorf("link %d = %#v", index, link)
		}
	}

	modelsURI := string(wantURI)
	if err := features.documents.Open(modelsURI, 1, navigationModelsSource); err != nil {
		t.Fatal(err)
	}
	links, err = features.DocumentLinks(modelsURI)
	if err != nil || len(links) != 2 {
		t.Fatalf("model DocumentLinks() = %#v, %v", links, err)
	}
	for _, link := range links {
		if link.Target == nil || *link.Target != wantURI {
			t.Errorf("relation link = %#v", link)
		}
	}
	if got := navigationModelsSource[byteOffsetForProtocolPosition(t, []byte(navigationModelsSource), links[0].Range.Start):byteOffsetForProtocolPosition(t, []byte(navigationModelsSource), links[0].Range.End)]; got != "Author" {
		t.Errorf("relation target link text = %q", got)
	}
	if got := navigationModelsSource[byteOffsetForProtocolPosition(t, []byte(navigationModelsSource), links[1].Range.Start):byteOffsetForProtocolPosition(t, []byte(navigationModelsSource), links[1].Range.End)]; got != "books" {
		t.Errorf("related_name link text = %q", got)
	}
	staleDeclaration := strings.Replace(navigationModelsSource, "\"Author\"", "\"Publisher\"", 1)
	if err := features.documents.Change(modelsURI, 2, []analysis.Change{{Text: staleDeclaration}}); err != nil {
		t.Fatal(err)
	}
	if links, err := features.DocumentLinks(modelsURI); err != nil || len(links) != 0 {
		t.Fatalf("stale declaration links = %#v, %v", links, err)
	}
}

func TestDocumentLinksRecognizeImportedRelationConstructor(t *testing.T) {
	source := strings.Replace(navigationModelsSource, "from django.db import models", "from django.db.models import ForeignKey as FK", 1)
	source = strings.Replace(source, "models.ForeignKey", "FK", 1)
	features, modelsPath := navigationTestFeaturesWithSource(t, source)
	defer features.Close()
	uri := mustSourceFileURI(t, modelsPath)
	if err := features.documents.Open(uri, 1, source); err != nil {
		t.Fatal(err)
	}
	links, err := features.DocumentLinks(uri)
	if err != nil || len(links) != 2 {
		t.Fatalf("imported-constructor DocumentLinks() = %#v, %v", links, err)
	}
}

func TestSourceLocationConvertsUTF8ColumnsAndEscapesURI(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "a # café")
	if err := os.Mkdir(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "model %.py")
	source := []byte("é😀class Model:\r\n\tvalue = 1\r\n")
	if err := os.WriteFile(path, source, 0o644); err != nil {
		t.Fatal(err)
	}
	digestValue := sha256.Sum256(source)
	digest := hex.EncodeToString(digestValue[:])
	location, ok := sourceLocation(schema.SourceRange{
		FilePath:     path,
		SourceDigest: digest,
		Start:        schema.Position{Line: 1, Column: 6},
		End:          schema.Position{Line: 2, Column: 10},
	})
	if !ok {
		t.Fatal("sourceLocation() returned no location")
	}
	wantRange := protocol.Range{Start: protocol.Position{Character: 3}, End: protocol.Position{Line: 1, Character: 10}}
	if location.Range != wantRange {
		t.Fatalf("range = %#v, want %#v", location.Range, wantRange)
	}
	for _, escaped := range []string{"%20", "%23", "%C3%A9", "%25"} {
		if !strings.Contains(string(location.URI), escaped) {
			t.Errorf("URI %q missing %q", location.URI, escaped)
		}
	}
	if _, ok := sourceLocation(schema.SourceRange{FilePath: path, SourceDigest: digest, Start: schema.Position{Line: 1, Column: 1}, End: schema.Position{Line: 1, Column: 2}}); ok {
		t.Fatal("mid-rune source column was accepted")
	}
}

func navigationTestFeatures(t navigationTestingT) (*Features, string) {
	return navigationTestFeaturesWithSource(t, navigationModelsSource)
}

func navigationTestFeaturesWithSource(t navigationTestingT, modelsSource string) (*Features, string) {
	t.Helper()
	directory := t.TempDir()
	modelsPath := filepath.Join(directory, "models.py")
	if err := os.WriteFile(modelsPath, []byte(modelsSource), 0o644); err != nil {
		t.Fatal(err)
	}
	digestValue := sha256.Sum256([]byte(modelsSource))
	digest := hex.EncodeToString(digestValue[:])
	rangeAt := func(startLine, startColumn, endLine, endColumn int) *schema.SourceRange {
		return &schema.SourceRange{FilePath: modelsPath, SourceDigest: digest, Start: schema.Position{Line: startLine, Column: startColumn}, End: schema.Position{Line: endLine, Column: endColumn}}
	}
	forward, reverse := "forward", "reverse"
	manyToOne, oneToMany := "many-to-one", "one-to-many"
	authorLabel, bookLabel := "myapp.Author", "myapp.Book"
	books := "books"
	querySetClass := "myapp.models.BookQuerySet"
	methodRange := rangeAt(6, 4, 6, 30)
	modelLines := strings.Split(modelsSource, "\n")
	authorFieldRange := rangeAt(5, 4, 5, len(modelLines[4]))
	managerRange := rangeAt(4, 0, 6, 30)
	models := map[string]schema.Model{
		"Author": {
			CanonicalLabel: authorLabel, Module: "myapp.models", Qualname: "Author", FilePath: modelsPath, LineNumber: 2,
			SourceRange: rangeAt(2, 0, 3, 29), Managed: true, DefaultManager: "objects", BaseManager: schema.BaseManager{Name: "_base_manager", OwnerClass: "django.db.models.Manager"},
			Managers: []schema.Manager{{Name: "objects", OwnerClass: "django.db.models.Manager", SourceRange: rangeAt(2, 0, 3, 29)}},
			Fields: map[string]schema.Field{
				"name": {Type: "django.db.models.CharField", InternalType: "CharField", Name: "name", SourceModel: authorLabel, SourceRange: rangeAt(3, 4, 3, 29), LookupPaths: []schema.LookupPath{{Lookups: []string{"icontains"}}}},
				"book": {Type: "django.db.models.ManyToOneRel", InternalType: "ManyToOneRel", Name: "book", IsRelation: true, RelatedModel: &bookLabel, SourceModel: bookLabel, SourceRange: authorFieldRange, RelationDirection: &reverse, RelationCardinality: &oneToMany, AccessorName: &books},
			},
		},
		"Book": {
			CanonicalLabel: bookLabel, Module: "myapp.models", Qualname: "Book", FilePath: modelsPath, LineNumber: 4,
			SourceRange: managerRange, Managed: true, DefaultManager: "objects", BaseManager: schema.BaseManager{Name: "_base_manager", OwnerClass: "django.db.models.Manager"},
			Managers: []schema.Manager{
				{Name: "objects", OwnerClass: "django.db.models.Manager", QuerySetClass: &querySetClass, SourceRange: managerRange, QuerySetMethods: []schema.BoundQuerySetMethod{{Method: schema.Method{Name: "active", OwnerClass: querySetClass, SourceRange: methodRange, Chainable: true}, AvailableOnManager: true}}},
				{Name: "catalog", OwnerClass: "myapp.models.BookManager", SourceRange: managerRange, Methods: []schema.Method{{Name: "featured", OwnerClass: "myapp.models.BookManager", SourceRange: methodRange, Chainable: true}}},
			},
			QuerySetMethods: []schema.Method{{Name: "active", OwnerClass: querySetClass, SourceRange: methodRange, Chainable: true}},
			Fields: map[string]schema.Field{
				"author": {Type: "django.db.models.ForeignKey", InternalType: "ForeignKey", Name: "author", IsRelation: true, RelatedModel: &authorLabel, RuntimeRelatedModel: &authorLabel, SourceModel: bookLabel, SourceRange: authorFieldRange, RelationDirection: &forward, RelationCardinality: &manyToOne, LookupPaths: []schema.LookupPath{{Lookups: []string{"exact"}}}},
				"title":  {Type: "django.db.models.CharField", InternalType: "CharField", Name: "title", SourceModel: bookLabel, SourceRange: rangeAt(6, 4, 6, 30), LookupPaths: []schema.LookupPath{{Transforms: []string{"lower"}, Kinds: []string{"transform"}, Lookups: []string{"icontains"}}, {Lookups: []string{"icontains"}}}},
			},
		},
	}
	graph, err := schema.Build(schema.Snapshot{SchemaVersion: 1, PositionEncoding: "utf-8-bytes", LookupTransformMaxDepth: 2, LookupPathMaxCount: 512, Apps: map[string]schema.App{"myapp": {Label: "myapp", ImportName: "myapp", RootPath: directory, Models: models}}})
	if err != nil {
		t.Fatal(err)
	}
	cache := &schema.Cache{}
	cache.Replace(graph)
	features, err := NewFeatures(cache)
	if err != nil {
		t.Fatal(err)
	}
	return features, modelsPath
}

func BenchmarkDefinitionHandler(b *testing.B) {
	features, _ := navigationTestFeatures(b)
	defer features.Close()
	uri := "file:///workspace/definition-benchmark.py"
	source, position := lspSourceAtCursor(b, "from myapp.models import Book\nBook.objects.filter(author__na|me=1)")
	if err := features.documents.Open(uri, 1, string(source)); err != nil {
		b.Fatal(err)
	}
	benchmarkNavigationLatency(b, func() {
		if location, err := features.Definition(uri, position); err != nil || location == nil {
			b.Fatalf("Definition() = %#v, %v", location, err)
		}
	})
}

func BenchmarkDocumentLinkHandler(b *testing.B) {
	features, _ := navigationTestFeatures(b)
	defer features.Close()
	uri := "file:///workspace/link-benchmark.py"
	source := "from myapp.models import Book\nBook.objects.values(\"author__name\", \"title__lower\")\n"
	if err := features.documents.Open(uri, 1, source); err != nil {
		b.Fatal(err)
	}
	benchmarkNavigationLatency(b, func() {
		if links, err := features.DocumentLinks(uri); err != nil || len(links) != 4 {
			b.Fatalf("DocumentLinks() = %#v, %v", links, err)
		}
	})
}

func benchmarkNavigationLatency(b *testing.B, run func()) {
	b.Helper()
	totals := make([]time.Duration, b.N)
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		started := time.Now()
		run()
		totals[index] = time.Since(started)
	}
	b.StopTimer()
	sort.Slice(totals, func(left, right int) bool { return totals[left] < totals[right] })
	b.ReportMetric(float64(percentile(totals, 50).Nanoseconds())/1000, "p50-us")
	b.ReportMetric(float64(percentile(totals, 95).Nanoseconds())/1000, "p95-us")
	b.ReportMetric(float64(percentile(totals, 99).Nanoseconds())/1000, "p99-us")
}

type navigationTestingT interface {
	Helper()
	Fatal(args ...any)
	Fatalf(format string, args ...any)
	TempDir() string
}

func byteOffsetForProtocolPosition(t *testing.T, source []byte, position protocol.Position) int {
	t.Helper()
	offset, ok := analysis.ByteOffset(source, analysisPosition(position))
	if !ok {
		t.Fatalf("invalid protocol position %#v", position)
	}
	return offset
}
