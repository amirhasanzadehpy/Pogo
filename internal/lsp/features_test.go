package lsp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/amirhasanzadehpy/Pogo/internal/analysis"
	pythonworker "github.com/amirhasanzadehpy/Pogo/internal/python"
	"github.com/amirhasanzadehpy/Pogo/internal/schema"
	protocol "github.com/tliron/glsp/protocol_3_16"
)

func TestDirectCompletionAndHoverUseCachedGraph(t *testing.T) {
	features := testFeatures(t)
	defer features.Close()
	uri := "file:///workspace/example.py"
	source := "label = '😀'\nfrom myapp.models import Book\nBook.objects.filter(au)\n"
	if err := features.documents.Open(uri, 1, source); err != nil {
		t.Fatal(err)
	}

	completion, err := features.Completion(uri, analysis.Position{Line: 2, Character: 22})
	if err != nil {
		t.Fatalf("Completion() error = %v", err)
	}
	if completion == nil || len(completion.Items) != 2 {
		t.Fatalf("Completion() = %#v", completion)
	}
	if completion.Items[0].Label != "author" || completion.Items[1].Label != "author_id" {
		t.Fatalf("completion labels = %q, %q", completion.Items[0].Label, completion.Items[1].Label)
	}
	textEdit, ok := completion.Items[0].TextEdit.(protocol.TextEdit)
	if !ok {
		t.Fatalf("completion text edit type = %T", completion.Items[0].TextEdit)
	}
	if textEdit.NewText != "author" || textEdit.Range.Start != (protocol.Position{Line: 2, Character: 20}) || textEdit.Range.End != (protocol.Position{Line: 2, Character: 22}) {
		t.Fatalf("completion text edit = %#v", textEdit)
	}

	if err := features.documents.Change(uri, 2, []analysis.Change{{
		Range: &analysis.Range{Start: analysis.Position{Line: 2, Character: 20}, End: analysis.Position{Line: 2, Character: 22}},
		Text:  "author",
	}}); err != nil {
		t.Fatal(err)
	}
	hover, err := features.Hover(uri, analysis.Position{Line: 2, Character: 23})
	if err != nil {
		t.Fatalf("Hover() error = %v", err)
	}
	if hover == nil {
		t.Fatal("Hover() = nil")
	}
	markup, ok := hover.Contents.(protocol.MarkupContent)
	if !ok || markup.Kind != protocol.MarkupKindMarkdown {
		t.Fatalf("hover contents = %#v", hover.Contents)
	}
	for _, expected := range []string{"**author**", "ForeignKey", "Database type: `bigint`", "Database column: `author_id`", "Null: `false`", "Indexed: `true`", "Unique: `false`", "Primary key: `false`", "Related model: `myapp.Author`", "Source model: `myapp.Book`", "Book author"} {
		if !strings.Contains(markup.Value, expected) {
			t.Errorf("hover markdown missing %q:\n%s", expected, markup.Value)
		}
	}
}

func TestManagerAndInstanceCompletion(t *testing.T) {
	features := testFeatures(t)
	defer features.Close()
	tests := []struct {
		uri      string
		source   string
		line     uint32
		column   uint32
		expected []string
	}{
		{"file:///model.py", "from myapp.models import Book\nBook.ob", 1, 7, []string{"objects"}},
		{"file:///instance.py", "from myapp.models import Book\nbook = Book.objects.first()\nbook.ti", 2, 7, []string{"title"}},
		{"file:///multiline.py", "from myapp.models import (\n    Book,\n)\nBook.ob", 3, 7, []string{"objects"}},
	}
	for index, test := range tests {
		if err := features.documents.Open(test.uri, int32(index+1), test.source); err != nil {
			t.Fatal(err)
		}
		completion, err := features.Completion(test.uri, analysis.Position{Line: test.line, Character: test.column})
		if err != nil || completion == nil {
			t.Fatalf("Completion(%s) = %#v, %v", test.uri, completion, err)
		}
		if len(completion.Items) != len(test.expected) {
			t.Fatalf("completion items = %#v", completion.Items)
		}
		for itemIndex, expected := range test.expected {
			if completion.Items[itemIndex].Label != expected {
				t.Errorf("completion item %d = %q, want %q", itemIndex, completion.Items[itemIndex].Label, expected)
			}
		}
	}
}

func TestCompletionReturnsNothingWithoutGraphOrInference(t *testing.T) {
	cache := &schema.Cache{}
	features, err := NewFeatures(cache)
	if err != nil {
		t.Fatal(err)
	}
	defer features.Close()
	uri := "file:///unknown.py"
	if err := features.documents.Open(uri, 1, "unknown.field"); err != nil {
		t.Fatal(err)
	}
	completion, err := features.Completion(uri, analysis.Position{Character: 13})
	if err != nil || completion != nil {
		t.Fatalf("Completion() = %#v, %v", completion, err)
	}
}

func TestDeepPathCompletionHoverAndCustomMethods(t *testing.T) {
	features := testFeatures(t)
	defer features.Close()
	tests := []struct {
		name   string
		source string
		want   []string
	}{
		{"lookup relation", "from myapp.models import Book\nBook.objects.filter(author__na|=value)", []string{"name"}},
		{"lookup transform", "from myapp.models import Book\nBook.objects.filter(published_at__date__g|=value)", []string{"gte"}},
		{"projection", "from myapp.models import Book\nBook.objects.values(\"title\", \"author__na|\")", []string{"name"}},
		{"only", "from myapp.models import Book\nBook.objects.only(\"author__na|\")", []string{"name"}},
		{"select related", "from myapp.models import Book\nBook.objects.select_related(\"author__pro|\")", []string{"profile"}},
		{"prefetch related", "from myapp.models import Book\nBook.objects.prefetch_related(\"sto|\")", []string{"store_set"}},
		{"queryset method", "from myapp.models import Book\nBook.objects.ac|", []string{"active"}},
		{"manager method", "from myapp.models import Book\nBook.catalog.fea|", []string{"featured"}},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			uri := "file:///deep-" + string(rune('a'+index)) + ".py"
			source, position := lspSourceAtCursor(t, test.source)
			if err := features.documents.Open(uri, 1, string(source)); err != nil {
				t.Fatal(err)
			}
			completion, err := features.Completion(uri, position)
			if err != nil || completion == nil {
				t.Fatalf("Completion() = %#v, %v", completion, err)
			}
			labels := make([]string, len(completion.Items))
			for itemIndex, item := range completion.Items {
				labels[itemIndex] = item.Label
			}
			if strings.Join(labels, ",") != strings.Join(test.want, ",") {
				t.Fatalf("labels = %v, want %v", labels, test.want)
			}
			edit, ok := completion.Items[0].TextEdit.(protocol.TextEdit)
			if !ok || edit.NewText != test.want[0] {
				t.Fatalf("text edit = %#v", completion.Items[0].TextEdit)
			}
		})
	}

	uri := "file:///deep-hover.py"
	source, position := lspSourceAtCursor(t, "from myapp.models import Book\nBook.objects.filter(published_at__date__gt|e=value)")
	if err := features.documents.Open(uri, 1, string(source)); err != nil {
		t.Fatal(err)
	}
	hover, err := features.Hover(uri, position)
	if err != nil || hover == nil {
		t.Fatalf("Hover() = %#v, %v", hover, err)
	}
	markup := hover.Contents.(protocol.MarkupContent)
	if !strings.Contains(markup.Value, "Django lookup: `gte`") || !strings.Contains(markup.Value, "DateTimeField") {
		t.Fatalf("hover markdown = %s", markup.Value)
	}

	uri = "file:///method-hover.py"
	source, position = lspSourceAtCursor(t, "from myapp.models import Book\nBook.objects.act|ive()")
	if err := features.documents.Open(uri, 1, string(source)); err != nil {
		t.Fatal(err)
	}
	hover, err = features.Hover(uri, position)
	if err != nil || hover == nil {
		t.Fatalf("method Hover() = %#v, %v", hover, err)
	}
	markup = hover.Contents.(protocol.MarkupContent)
	if !strings.Contains(markup.Value, "BookQuerySet.active") || !strings.Contains(markup.Value, activeMethodDoc) {
		t.Fatalf("method hover markdown = %s", markup.Value)
	}

	for index, sourceWithCursor := range []string{
		"from myapp.models import Book\nBook.objects.filter(author|__name=value)",
		"from myapp.models import Book\nBook.objects.filter(author_|_name=value)",
	} {
		uri = fmt.Sprintf("file:///separator-%d.py", index)
		source, position = lspSourceAtCursor(t, sourceWithCursor)
		if err := features.documents.Open(uri, 1, string(source)); err != nil {
			t.Fatal(err)
		}
		hover, err = features.Hover(uri, position)
		if err != nil || hover != nil {
			t.Fatalf("separator Hover() = %#v, %v", hover, err)
		}
	}
}

const activeMethodDoc = "Return books that are currently active."

func TestSignatureHelpUsesCachedSignature(t *testing.T) {
	features := testFeatures(t)
	defer features.Close()
	uri := "file:///signature.py"
	source, position := lspSourceAtCursor(t, "from myapp.models import Book\nBook.objects.active(limit=|)")
	if err := features.documents.Open(uri, 1, string(source)); err != nil {
		t.Fatal(err)
	}
	help, err := features.SignatureHelp(uri, position)
	if err != nil || help == nil || len(help.Signatures) != 1 {
		t.Fatalf("SignatureHelp() = %#v, %v", help, err)
	}
	if help.Signatures[0].Label != "active(status: str = 'active', *, limit: int = 10)" {
		t.Fatalf("signature label = %q", help.Signatures[0].Label)
	}
	if help.ActiveParameter == nil || *help.ActiveParameter != 1 {
		t.Fatalf("active parameter = %#v", help.ActiveParameter)
	}
}

func TestDeepCompletionEditRangeUsesUTF16(t *testing.T) {
	features := testFeatures(t)
	defer features.Close()
	uri := "file:///utf16-deep.py"
	source, position := lspSourceAtCursor(t, "from myapp.models import Book\nlabel = '😀'; Book.objects.filter(author__na|=value)")
	if err := features.documents.Open(uri, 1, string(source)); err != nil {
		t.Fatal(err)
	}
	completion, err := features.Completion(uri, position)
	if err != nil || completion == nil || len(completion.Items) != 1 {
		t.Fatalf("Completion() = %#v, %v", completion, err)
	}
	edit := completion.Items[0].TextEdit.(protocol.TextEdit)
	startOffset := strings.Index(string(source), "author__na") + len("author__")
	wantStart, _ := analysis.PositionAt(source, startOffset)
	wantEnd, _ := analysis.PositionAt(source, startOffset+len("na"))
	if edit.Range.Start != protocolPosition(wantStart) || edit.Range.End != protocolPosition(wantEnd) {
		t.Fatalf("UTF-16 edit range = %#v, want %v..%v", edit.Range, wantStart, wantEnd)
	}
}

func TestCompletionAndHoverDoNotRequestStoppedWorker(t *testing.T) {
	project, err := filepath.Abs("../../testdata/sample_django_project")
	if err != nil {
		t.Fatal(err)
	}
	pythonPath, err := filepath.Abs("../../.venv-fixture/bin/python")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(pythonPath); err != nil {
		t.Skip("fixture environment is not installed")
	}
	cache := &schema.Cache{}
	manager, err := pythonworker.NewManager(pythonworker.Config{
		ProjectRoot: project, PythonPath: pythonPath, SettingsModule: "sample_project.settings",
		ConnectTimeout: 5 * time.Second, RequestTimeout: 10 * time.Second,
	}, cache, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	failure := make(chan error, 1)
	manager.Start(ctx, func(err error) { failure <- err })
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	for {
		if graph, generation := cache.Load(); graph != nil && generation > 0 {
			break
		}
		select {
		case err := <-failure:
			t.Fatalf("worker failed before schema load: %v", err)
		case <-deadline.C:
			t.Fatal("timed out waiting for schema load")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := manager.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	requestsAfterStop := manager.RequestCount()

	features, err := NewFeatures(cache)
	if err != nil {
		t.Fatal(err)
	}
	defer features.Close()
	uri := "file:///workspace/hot-path.py"
	source := []byte("from myapp.models import Book\nBook.objects.filter(author__na=1)\nBook.objects.active()\n")
	if err := features.documents.Open(uri, 1, string(source)); err != nil {
		t.Fatal(err)
	}
	completionOffset := strings.Index(string(source), "author__na") + len("author__na")
	completionPosition, _ := analysis.PositionAt(source, completionOffset)
	completion, err := features.Completion(uri, completionPosition)
	if err != nil || completion == nil || len(completion.Items) == 0 {
		t.Fatalf("Completion() with stopped worker = %#v, %v", completion, err)
	}
	hoverOffset := strings.Index(string(source), "author__na") + 2
	hoverPosition, _ := analysis.PositionAt(source, hoverOffset)
	hover, err := features.Hover(uri, hoverPosition)
	if err != nil || hover == nil {
		t.Fatalf("Hover() with stopped worker = %#v, %v", hover, err)
	}
	signatureOffset := strings.Index(string(source), "active(") + len("active(")
	signaturePosition, _ := analysis.PositionAt(source, signatureOffset)
	help, err := features.SignatureHelp(uri, signaturePosition)
	if err != nil || help == nil {
		t.Fatalf("SignatureHelp() with stopped worker = %#v, %v", help, err)
	}
	if got := manager.RequestCount(); got != requestsAfterStop {
		t.Fatalf("worker requests after completion/hover/signature = %d, want %d", got, requestsAfterStop)
	}
}

func BenchmarkCompletionLatency(b *testing.B) {
	features := testFeatures(b)
	defer features.Close()
	uri := "file:///benchmark.py"
	if err := features.documents.Open(uri, 1, "from myapp.models import Book\nBook.objects.filter(author__na)\n"); err != nil {
		b.Fatal(err)
	}
	version := int32(1)
	totals := make([]time.Duration, b.N)
	parseTotal := time.Duration(0)
	handlerTotal := time.Duration(0)
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		started := time.Now()
		version++
		text := "nam"
		end := uint32(30)
		if index%2 == 1 {
			text = "na"
			end = 31
		}
		parseStarted := time.Now()
		if err := features.documents.Change(uri, version, []analysis.Change{{
			Range: &analysis.Range{
				Start: analysis.Position{Line: 1, Character: 28},
				End:   analysis.Position{Line: 1, Character: end},
			},
			Text: text,
		}}); err != nil {
			b.Fatal(err)
		}
		parseTotal += time.Since(parseStarted)
		handlerStarted := time.Now()
		if _, err := features.Completion(uri, analysis.Position{Line: 1, Character: uint32(28 + len(text))}); err != nil {
			b.Fatal(err)
		}
		handlerTotal += time.Since(handlerStarted)
		totals[index] = time.Since(started)
	}
	b.StopTimer()
	sort.Slice(totals, func(left, right int) bool { return totals[left] < totals[right] })
	b.ReportMetric(float64(parseTotal.Nanoseconds())/float64(b.N), "parse-ns/op")
	b.ReportMetric(float64(handlerTotal.Nanoseconds())/float64(b.N), "handler-ns/op")
	b.ReportMetric(float64(percentile(totals, 50).Nanoseconds())/1000, "p50-us")
	p95 := percentile(totals, 95)
	b.ReportMetric(float64(p95.Nanoseconds())/1000, "p95-us")
	if p95 >= 10*time.Millisecond {
		b.Fatalf("loaded-cache deep completion p95 = %s, want < 10ms", p95)
	}
}

func BenchmarkParseUpdate(b *testing.B) {
	features := testFeatures(b)
	defer features.Close()
	uri := "file:///parse-benchmark.py"
	if err := features.documents.Open(uri, 1, "from myapp.models import Book\nBook.objects.filter(author__na)\n"); err != nil {
		b.Fatal(err)
	}
	version := int32(1)
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		version++
		text, end := "nam", uint32(30)
		if index%2 == 1 {
			text, end = "na", 31
		}
		if err := features.documents.Change(uri, version, []analysis.Change{{
			Range: &analysis.Range{Start: analysis.Position{Line: 1, Character: 28}, End: analysis.Position{Line: 1, Character: end}},
			Text:  text,
		}}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCompletionHandler(b *testing.B) {
	features := testFeatures(b)
	defer features.Close()
	uri := "file:///handler-benchmark.py"
	if err := features.documents.Open(uri, 1, "from myapp.models import Book\nBook.objects.filter(author__na)\n"); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := features.Completion(uri, analysis.Position{Line: 1, Character: 30}); err != nil {
			b.Fatal(err)
		}
	}
}

func percentile(values []time.Duration, percent int) time.Duration {
	if len(values) == 0 {
		return 0
	}
	index := (len(values)*percent + 99) / 100
	if index > 0 {
		index--
	}
	return values[index]
}

type testingT interface {
	Helper()
	Fatal(args ...any)
	Fatalf(format string, args ...any)
}

func testFeatures(t testingT) *Features {
	t.Helper()
	graph := featureTestGraph(t)
	cache := &schema.Cache{}
	cache.Replace(graph)
	features, err := NewFeatures(cache)
	if err != nil {
		t.Fatal(err)
	}
	return features
}

func featureTestGraph(t testingT) *schema.Graph {
	t.Helper()
	attname := "author_id"
	dbColumn := "author_id"
	dbType := "bigint"
	related := "myapp.Author"
	forward := "forward"
	reverse := "reverse"
	manyToOne := "many-to-one"
	oneToOne := "one-to-one"
	manyToMany := "many-to-many"
	profileLabel := "myapp.Profile"
	storeLabel := "myapp.Store"
	profileName := "profile"
	storeQuery := "store"
	storeAccessor := "store_set"
	querySetClass := "myapp.models.BookQuerySet"
	activeSignature := "(status: str = 'active', *, limit: int = 10)"
	activeDoc := "Return books that are currently active."
	featuredSignature := "() -> models.QuerySet['Book']"
	title := featureTestField("myapp.Book", "title", "django.db.models.fields.CharField")
	title.LookupPaths = []schema.LookupPath{{Lookups: []string{"exact", "icontains", "startswith"}}}
	published := featureTestField("myapp.Book", "published_at", "django.db.models.fields.DateTimeField")
	published.LookupPaths = []schema.LookupPath{
		{Lookups: []string{"exact", "gte"}},
		{Transforms: []string{"date"}, Kinds: []string{"transform"}, Lookups: []string{"exact", "gte"}},
	}
	storeRelation := schema.Field{
		Type: "django.db.models.fields.reverse_related.ManyToManyRel", InternalType: "ManyToManyRel", Name: "store",
		IsRelation: true, RelatedModel: &storeLabel, RelationDirection: &reverse, RelationCardinality: &manyToMany,
		QueryName: &storeQuery, AccessorName: &storeAccessor, SourceModel: "myapp.Store", SourceRange: featureTestSourceRange(),
	}
	graph, err := schema.Build(schema.Snapshot{
		SchemaVersion: 1, PositionEncoding: "utf-8-bytes", LookupTransformMaxDepth: 2, LookupPathMaxCount: 512,
		Apps: map[string]schema.App{"myapp": {
			Label: "myapp", ImportName: "myapp", RootPath: "/project/myapp",
			Models: map[string]schema.Model{
				"Author": featureTestModel("Author", map[string]schema.Field{
					"id":   featureTestField("myapp.Author", "id", "django.db.models.fields.BigAutoField"),
					"name": featureTestField("myapp.Author", "name", "django.db.models.fields.CharField"),
					"profile": {
						Type: "django.db.models.fields.reverse_related.OneToOneRel", InternalType: "OneToOneRel", Name: "profile",
						IsRelation: true, RelatedModel: &profileLabel, RelationDirection: &reverse, RelationCardinality: &oneToOne,
						QueryName: &profileName, AccessorName: &profileName, SourceModel: "myapp.Profile", SourceRange: featureTestSourceRange(),
					},
				}),
				"Profile": featureTestModel("Profile", map[string]schema.Field{
					"display_name": featureTestField("myapp.Profile", "display_name", "django.db.models.fields.CharField"),
				}),
				"Store": featureTestModel("Store", map[string]schema.Field{
					"name": featureTestField("myapp.Store", "name", "django.db.models.fields.CharField"),
				}),
				"Book": func() schema.Model {
					model := featureTestModel("Book", map[string]schema.Field{
						"id":           featureTestField("myapp.Book", "id", "django.db.models.fields.BigAutoField"),
						"title":        title,
						"published_at": published,
						"store":        storeRelation,
						"author": {
							Type: "django.db.models.fields.related.ForeignKey", InternalType: "ForeignKey", Name: "author", Attname: &attname,
							DBType: &dbType, DBColumn: &dbColumn, DBIndex: true, HelpText: "Book author", IsRelation: true,
							RelatedModel: &related, SourceModel: "myapp.Book", SourceRange: featureTestSourceRange(), RelationDirection: &forward, RelationCardinality: &manyToOne,
							LookupPaths: []schema.LookupPath{{Lookups: []string{"exact", "in", "isnull"}}},
						},
					})
					model.Managers = []schema.Manager{
						{Name: "objects", OwnerClass: "django.db.models.ManagerFromBookQuerySet", QuerySetClass: &querySetClass, SourceRange: featureTestSourceRange(), QuerySetMethods: []schema.BoundQuerySetMethod{{Method: schema.Method{Name: "active", OwnerClass: querySetClass, Signature: &activeSignature, Docstring: &activeDoc, Chainable: true, AssumedChainable: true, SourceRange: featureTestSourceRange()}, AvailableOnManager: true}}},
						{Name: "catalog", OwnerClass: "myapp.models.BookManager", SourceRange: featureTestSourceRange(), Methods: []schema.Method{{Name: "featured", OwnerClass: "myapp.models.BookManager", Signature: &featuredSignature, Chainable: true, SourceRange: featureTestSourceRange()}}},
					}
					model.QuerySetMethods = []schema.Method{{Name: "active", OwnerClass: querySetClass, Signature: &activeSignature, Docstring: &activeDoc, Chainable: true, AssumedChainable: true, SourceRange: featureTestSourceRange()}}
					return model
				}(),
			},
		}},
	})
	if err != nil {
		t.Fatalf("schema.Build() error = %v", err)
	}
	return graph
}

func featureTestModel(name string, fields map[string]schema.Field) schema.Model {
	return schema.Model{
		CanonicalLabel: "myapp." + name, Module: "myapp.models", Qualname: name,
		FilePath: "/project/myapp/models.py", LineNumber: 1, SourceRange: featureTestSourceRange(), Managed: true,
		DefaultManager: "objects", BaseManager: schema.BaseManager{Name: "_base_manager", OwnerClass: "django.db.models.Manager"},
		Managers: []schema.Manager{{Name: "objects", OwnerClass: "django.db.models.Manager", SourceRange: featureTestSourceRange()}},
		Fields:   fields,
	}
}

func featureTestField(sourceModel, name, fieldType string) schema.Field {
	return schema.Field{Type: fieldType, InternalType: name, Name: name, SourceModel: sourceModel, SourceRange: featureTestSourceRange()}
}

func featureTestSourceRange() *schema.SourceRange {
	return &schema.SourceRange{FilePath: "/project/myapp/models.py", Start: schema.Position{Line: 1}, End: schema.Position{Line: 1, Column: 1}}
}

func lspSourceAtCursor(t *testing.T, value string) ([]byte, analysis.Position) {
	t.Helper()
	if strings.Count(value, "|") != 1 {
		t.Fatalf("source must contain one cursor: %q", value)
	}
	offset := strings.IndexByte(value, '|')
	source := []byte(value[:offset] + value[offset+1:])
	position, ok := analysis.PositionAt(source, offset)
	if !ok {
		t.Fatal("cursor position is invalid")
	}
	return source, position
}
