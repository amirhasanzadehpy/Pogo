package analysis

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/amirhasanzadehpy/Pogo/internal/schema"
)

func TestAnalyzeAndCompleteORMPaths(t *testing.T) {
	graph := pathTestGraph(t)
	tests := []struct {
		name   string
		source string
		want   []string
	}{
		{"deep relation", "from myapp.models import Book\nBook.objects.filter(author__na|=value)", []string{"name"}},
		{"Unicode relation field", "from myapp.models import Book\nBook.objects.filter(author__café|=value)", []string{"café"}},
		{"attname relation", "from myapp.models import Book\nBook.objects.filter(author_id__na|=value)", []string{"name"}},
		{"datetime transform", "from myapp.models import Book\nBook.objects.filter(published_at__da|=value)", []string{"date"}},
		{"transformed lookup", "from myapp.models import Book\nBook.objects.filter(published_at__date__g|=value)", []string{"gte"}},
		{"projection relation", "from myapp.models import Book\nBook.objects.values(\"title\", \"author__na|\")", []string{"name"}},
		{"projection transform", "from myapp.models import Book\nBook.objects.values(\"published_at__da|\")", []string{"date"}},
		{"field mask", "from myapp.models import Book\nBook.objects.only(\"author__na|\")", []string{"name"}},
		{"select related", "from myapp.models import Book\nBook.objects.select_related(\"author__pro|\")", []string{"profile"}},
		{"prefetch accessor", "from myapp.models import Book\nBook.objects.prefetch_related(\"sto|\")", []string{"store_set"}},
		{"recursive self relation", "from myapp.models import Node\nNode.objects.filter(parent__parent__na|=value)", []string{"name"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source, offset := sourceAtCursor(t, test.source)
			context, ok := Analyze(source, offset, graph)
			if !ok || context.Kind != ContextORMPath || context.Path == nil {
				t.Fatalf("Analyze() = %#v, %v", context, ok)
			}
			candidates := CompletePath(graph, context.Value.CanonicalLabel, context.Path.Mode, context.Path.Segments, context.Path.ActiveSegment)
			got := make([]string, len(candidates))
			for index, candidate := range candidates {
				got[index] = candidate.Name
			}
			if !slices.Equal(got, test.want) {
				t.Fatalf("candidates = %v, want %v", got, test.want)
			}
		})
	}
}

func TestModelAdminGetQuerySetPaths(t *testing.T) {
	graph := pathTestGraph(t)
	tests := []struct {
		name string
		path string
		want []string
	}{
		{name: "select related", path: `"author__pro|"`, want: []string{"profile"}},
		{name: "prefetch related", path: `"author__bo|"`, want: []string{"books"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source, offset := sourceAtCursor(t, modelAdminQuerySetSource(test.path))
			store := newTestStore(t)
			if err := store.Open("file:///admin.py", 1, string(source)); err != nil {
				t.Fatal(err)
			}
			snapshot, _ := store.Snapshot("file:///admin.py")
			context, ok := AnalyzeSyntaxFile(snapshot.Source, offset, graph, snapshot.Syntax, snapshot.FilePath)
			if !ok || context.Kind != ContextORMPath || context.Path == nil || context.Value.CanonicalLabel != "myapp.Book" {
				t.Fatalf("AnalyzeSyntaxFile() = %#v, %v", context, ok)
			}
			candidates := CompletePath(graph, context.Value.CanonicalLabel, context.Path.Mode, context.Path.Segments, context.Path.ActiveSegment)
			got := make([]string, len(candidates))
			for index, candidate := range candidates {
				got[index] = candidate.Name
			}
			if !slices.Equal(got, test.want) {
				t.Fatalf("candidates = %v, want %v", got, test.want)
			}
		})
	}
}

func TestModelAdminGetQuerySetPathNavigation(t *testing.T) {
	graph := pathTestGraph(t)
	source, offset := sourceAtCursor(t, modelAdminQuerySetSource(`"author__pro|file"`))
	store := newTestStore(t)
	if err := store.Open("file:///admin.py", 1, string(source)); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := store.Snapshot("file:///admin.py")
	if sourceRange, ok := ResolveDefinitionSyntaxFile(snapshot.Source, offset, graph, snapshot.Syntax, snapshot.FilePath); !ok || sourceRange.FilePath == "" {
		t.Fatalf("ResolveDefinitionSyntaxFile() = %#v, %v", sourceRange, ok)
	}

}

func TestModelAdminGetQuerySetRequiresDjangoAdminRegistration(t *testing.T) {
	graph := pathTestGraph(t)
	source, offset := sourceAtCursor(t, strings.Replace(modelAdminQuerySetSource(`"author__pro|"`), "@admin.register(Book)", "@registry.register(Book)", 1))
	store := newTestStore(t)
	if err := store.Open("file:///admin.py", 1, string(source)); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := store.Snapshot("file:///admin.py")
	if context, ok := AnalyzeSyntaxFile(snapshot.Source, offset, graph, snapshot.Syntax, snapshot.FilePath); ok {
		t.Fatalf("unrelated register decorator inferred as %#v", context)
	}
}

func modelAdminQuerySetSource(activePath string) string {
	selectPath := `"author__profile"`
	prefetchPath := `"author__books"`
	if strings.Contains(activePath, "pro") {
		selectPath = activePath
	} else {
		prefetchPath = activePath
	}
	return "from django.contrib import admin\n" +
		"from myapp.models import Book\n\n" +
		"@admin.register(Book)\n" +
		"class BookAdmin(admin.ModelAdmin):\n" +
		"    def get_queryset(self, request):\n" +
		"        return (\n" +
		"            super()\n" +
		"            .get_queryset(request)\n" +
		"            .select_related(" + selectPath + ")\n" +
		"            .prefetch_related(" + prefetchPath + ")\n" +
		"        )\n"
}

func FuzzORMPathExtraction(f *testing.F) {
	for _, seed := range []string{
		"Book.objects.filter(author__name=value)",
		"Book.objects.values(\"author__name\")",
		"Book.objects.filter(parent__parent__name=value)",
		"Book.objects.filter(value=func(\"comma,inside\"), author__name=value)",
		"Book.objects.values(\"\"\"author__name\"\"\")",
	} {
		f.Add("from myapp.models import Book\n"+seed, uint16(len(seed)))
	}
	f.Fuzz(func(t *testing.T, text string, rawOffset uint16) {
		source := []byte(text)
		if len(source) == 0 || len(source) > 64*1024 || !utf8.Valid(source) {
			t.Skip()
		}
		offset := int(rawOffset) % (len(source) + 1)
		context, ok := Analyze(source, offset, pathTestGraph(t))
		if !ok || context.Path == nil {
			return
		}
		path := context.Path
		for _, range_ := range []ByteRange{context.Replacement, path.Replacement, path.Arguments} {
			if range_.Start < 0 || range_.Start > range_.End || range_.End > len(source) {
				t.Fatalf("out-of-bounds range %#v for %d-byte source", range_, len(source))
			}
		}
		if path.ActiveSegment < 0 || path.ActiveSegment >= len(path.Segments) {
			t.Fatalf("active segment %d outside %d segments", path.ActiveSegment, len(path.Segments))
		}
		for _, segment := range path.Segments {
			if segment.Range.Start < 0 || segment.Range.Start > segment.Range.End || segment.Range.End > len(source) || string(source[segment.Range.Start:segment.Range.End]) != segment.Text {
				t.Fatalf("invalid segment %#v", segment)
			}
		}
	})
}

func TestPathResolverContextPolicies(t *testing.T) {
	graph := pathTestGraph(t)
	tests := []struct {
		name  string
		mode  PathMode
		path  string
		valid bool
		kind  PathCandidateKind
	}{
		{"lookup", PathLookup, "published_at__date__gte", true, PathCandidateLookup},
		{"invalid transform", PathLookup, "published_at__date__hour", false, 0},
		{"lookup is terminal", PathLookup, "title__icontains__exact", false, 0},
		{"JSON key", PathLookup, "metadata__featured", true, PathCandidateTransform},
		{"JSON nested lookup", PathLookup, "metadata__nested__label__icontains", true, PathCandidateLookup},
		{"projection transform", PathProjection, "published_at__date", true, PathCandidateTransform},
		{"projection rejects lookup", PathProjection, "title__icontains", false, 0},
		{"only rejects transform", PathFields, "published_at__date", false, 0},
		{"select forward FK", PathSelectRelated, "author__profile", true, PathCandidateField},
		{"select rejects M2M", PathSelectRelated, "store", false, 0},
		{"prefetch reverse accessor", PathPrefetchRelated, "store_set", true, PathCandidateField},
		{"prefetch rejects query name", PathPrefetchRelated, "store", false, 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			segments := pathSegments(test.path)
			resolved, ok := ResolvePathSegment(graph, "myapp.Book", test.mode, segments, len(segments)-1)
			if ok != test.valid {
				t.Fatalf("ResolvePathSegment(%q) = %#v, %v", test.path, resolved, ok)
			}
			if ok && resolved.Kind != test.kind {
				t.Fatalf("kind = %v, want %v", resolved.Kind, test.kind)
			}
		})
	}
}

func TestCustomManagerAndQuerySetInference(t *testing.T) {
	graph := pathTestGraph(t)
	tests := []struct {
		source string
		want   string
		ok     bool
	}{
		{"from myapp.models import Book\nBook.objects.ac|", "active", true},
		{"from myapp.models import Book\nBook.objects.active().ac|", "active", true},
		{"from myapp.models import Book\nBook.catalog.fea|", "featured", true},
		{"from myapp.models import Book\nBook.catalog.featured().ac|", "", false},
		{"from myapp.models import Book\nBook.objects.featured().ac|", "active", true},
		{"from myapp.models import Book\nBook.objects.hid|", "", false},
		{"from myapp.models import Book\nBook.objects.all().hid|", "hidden", true},
	}
	for _, test := range tests {
		source, offset := sourceAtCursor(t, test.source)
		context, ok := Analyze(source, offset, graph)
		if !ok || context.Kind != ContextMethodMember {
			t.Fatalf("Analyze(%q) = %#v, %v", test.source, context, ok)
		}
		var got []string
		VisitMethods(graph, context.Value, func(method *schema.MethodRef) bool {
			if len(context.Identifier) <= len(method.Name()) && method.Name()[:len(context.Identifier)] == context.Identifier {
				got = append(got, method.Name())
			}
			return true
		})
		if test.ok && !slices.Contains(got, test.want) || !test.ok && len(got) != 0 {
			t.Fatalf("methods = %v", got)
		}
	}
}

func TestAnalyzeSignatureUsesCachedMethod(t *testing.T) {
	graph := pathTestGraph(t)
	for _, sourceWithCursor := range []string{
		"from myapp.models import Book\nBook.objects.active(status=|)",
		"from myapp.models import Book\nBook.objects.active(sta|tus=value)",
	} {
		source, offset := sourceAtCursor(t, sourceWithCursor)
		context, ok := AnalyzeSignatureSyntax(source, offset, graph, nil)
		if !ok || context.Method.Name() != "active" || context.Keyword != "status" || context.PositionalIndex != 0 {
			t.Fatalf("AnalyzeSignatureSyntax() = %#v, %v", context, ok)
		}
	}
}

func TestPositionalArgumentIndexIgnoresCommentCommas(t *testing.T) {
	source := []byte("first, # explanatory, comma\nsecond")
	activeStart := len(source) - len("second")
	if got := positionalArgumentIndex(source, 0, activeStart); got != 1 {
		t.Fatalf("positionalArgumentIndex() = %d, want 1", got)
	}
}

func TestCustomBuiltinOverrideSuppressesORMPathContext(t *testing.T) {
	graph := pathTestGraph(t)
	source, offset := sourceAtCursor(t, "from myapp.models import Book\nBook.catalog.filter(author__na|=value)")
	if context, ok := Analyze(source, offset, graph); ok {
		t.Fatalf("custom filter override inferred as %#v", context)
	}
}

func pathSegments(path string) []PathSegment {
	parts := []PathSegment{}
	start := 0
	for index := 0; index <= len(path); index++ {
		if index == len(path) || index+1 < len(path) && path[index:index+2] == "__" {
			parts = append(parts, PathSegment{Text: path[start:index], Range: ByteRange{Start: start, End: index}})
			if index < len(path) {
				index++
				start = index + 1
			}
		}
	}
	return parts
}

func TestResolvePathSegmentRejectsNonterminalLookup(t *testing.T) {
	graph := pathTestGraph(t)
	segments := []PathSegment{{Text: "title"}, {Text: "icontains"}, {Text: "exact"}}
	if _, ok := ResolvePathSegment(graph, "myapp.Book", PathLookup, segments, 1); ok {
		t.Fatal("ResolvePathSegment() accepted a nonterminal lookup")
	}
}

func pathTestGraph(t *testing.T) *schema.Graph {
	t.Helper()
	forward, reverse := "forward", "reverse"
	manyToOne, oneToMany, oneToOne, manyToMany := "many-to-one", "one-to-many", "one-to-one", "many-to-many"
	bookLabel, authorLabel, profileLabel, storeLabel, nodeLabel := "myapp.Book", "myapp.Author", "myapp.Profile", "myapp.Store", "myapp.Node"
	books, book, profile, store, storeSet := "books", "book", "profile", "store", "store_set"
	authorID := "author_id"
	querySetClass := "myapp.models.BookQuerySet"
	activeSignature := "(status: str = 'active')"
	activeDoc := "Return active books."
	hiddenSignature := "()"
	featuredSignature := "() -> models.QuerySet['Book']"
	activeMethod := schema.Method{Name: "active", OwnerClass: querySetClass, Signature: &activeSignature, Docstring: &activeDoc, Chainable: true, AssumedChainable: true, SourceRange: testSourceRange()}
	hiddenMethod := schema.Method{Name: "hidden", OwnerClass: querySetClass, Signature: &hiddenSignature, Chainable: true, AssumedChainable: true, SourceRange: testSourceRange()}
	featuredMethod := schema.Method{Name: "featured", OwnerClass: "myapp.models.BookManager", Signature: &featuredSignature, Chainable: true, SourceRange: testSourceRange()}
	filterMethod := schema.Method{Name: "filter", OwnerClass: "myapp.models.BookManager", Signature: &hiddenSignature, SourceRange: testSourceRange()}
	authorName := testField("myapp.Author", "name", "django.db.models.fields.CharField")
	authorName.LookupPaths = []schema.LookupPath{{Lookups: []string{"exact", "icontains"}}}
	authorPrimaryKey := testField("myapp.Author", "id", "django.db.models.fields.BigAutoField")
	authorPrimaryKey.PrimaryKey = true
	authorPrimaryKey.EffectivePrimaryKey = true
	models := map[string]schema.Model{
		"Author": testModel("Author", map[string]schema.Field{
			"id":      authorPrimaryKey,
			"name":    authorName,
			"café":    testField("myapp.Author", "café", "django.db.models.fields.CharField"),
			"book":    relationField("myapp.Book", "book", bookLabel, reverse, oneToMany, &book, &books),
			"profile": relationField("myapp.Profile", "profile", profileLabel, reverse, oneToOne, &profile, &profile),
		}),
		"Profile": testModel("Profile", map[string]schema.Field{
			"display_name": testField("myapp.Profile", "display_name", "django.db.models.fields.CharField"),
		}),
		"Store": testModel("Store", map[string]schema.Field{"name": testField("myapp.Store", "name", "django.db.models.fields.CharField")}),
		"Node": testModel("Node", map[string]schema.Field{
			"name":   testField("myapp.Node", "name", "django.db.models.fields.CharField"),
			"parent": relationField("myapp.Node", "parent", nodeLabel, forward, manyToOne, nil, nil),
		}),
	}
	title := testField("myapp.Book", "title", "django.db.models.fields.CharField")
	title.LookupPaths = []schema.LookupPath{{Lookups: []string{"exact", "icontains"}}}
	title.UnsupportedLookups = []string{"contains"}
	published := testField("myapp.Book", "published_at", "django.db.models.fields.DateTimeField")
	published.LookupPaths = []schema.LookupPath{
		{Lookups: []string{"exact", "gte"}},
		{Transforms: []string{"date"}, Kinds: []string{"transform"}, Lookups: []string{"exact", "gte"}},
	}
	metadata := testField("myapp.Book", "metadata", "django.db.models.fields.json.JSONField")
	metadata.LookupPaths = []schema.LookupPath{
		{Lookups: []string{"exact", "icontains"}},
		{Transforms: []string{"*"}, Kinds: []string{"key_transform"}, Lookups: []string{"exact", "icontains"}},
		{Transforms: []string{"*", "*"}, Kinds: []string{"key_transform", "key_transform"}, Lookups: []string{"exact", "icontains"}},
	}
	author := relationField("myapp.Book", "author", authorLabel, forward, manyToOne, nil, nil)
	author.Attname = &authorID
	author.LookupPaths = []schema.LookupPath{{Lookups: []string{"exact", "in", "isnull"}}}
	storeRelation := relationField("myapp.Store", "store", storeLabel, reverse, manyToMany, &store, &storeSet)
	bookPrimaryKey := testField("myapp.Book", "id", "django.db.models.fields.BigAutoField")
	bookPrimaryKey.PrimaryKey = true
	bookPrimaryKey.EffectivePrimaryKey = true
	bookModel := testModel("Book", map[string]schema.Field{
		"id": bookPrimaryKey, "title": title, "published_at": published, "metadata": metadata, "author": author, "store": storeRelation,
	})
	bookModel.Managers = []schema.Manager{
		{Name: "objects", OwnerClass: "django.db.models.ManagerFromBookQuerySet", QuerySetClass: &querySetClass, SourceRange: testSourceRange(), Methods: []schema.Method{featuredMethod}, QuerySetMethods: []schema.BoundQuerySetMethod{{Method: activeMethod, AvailableOnManager: true}, {Method: hiddenMethod}}},
		{Name: "catalog", OwnerClass: "myapp.models.BookManager", SourceRange: testSourceRange(), Methods: []schema.Method{featuredMethod, filterMethod}},
	}
	bookModel.QuerySetMethods = []schema.Method{activeMethod, hiddenMethod}
	models["Book"] = bookModel
	graph, err := schema.Build(schema.Snapshot{
		SchemaVersion: 1, PositionEncoding: "utf-8-bytes", LookupTransformMaxDepth: 2, LookupPathMaxCount: 512,
		Apps: map[string]schema.App{"myapp": {Label: "myapp", ImportName: "myapp", RootPath: filepath.Dir(inferenceTestModelPath()), Models: models}},
	})
	if err != nil {
		t.Fatalf("schema.Build() error = %v", err)
	}
	return graph
}

func relationField(sourceModel, name, related, direction, cardinality string, query, accessor *string) schema.Field {
	return schema.Field{
		Type: "django.db.models.fields.related.ForeignKey", InternalType: "ForeignKey", Name: name,
		IsRelation: true, RelatedModel: &related, RelationDirection: &direction, RelationCardinality: &cardinality,
		QueryName: query, AccessorName: accessor, SourceModel: sourceModel, SourceRange: testSourceRange(),
	}
}
