package analysis

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/amirhasanzadehpy/Pogo/internal/schema"
)

func TestAnalyzeQueryKeywordAcrossInferenceForms(t *testing.T) {
	graph := inferenceTestGraph(t)
	tests := []struct {
		name   string
		source string
	}{
		{"direct import", "from myapp.models import Book\nBook.objects.filter(au|"},
		{"aliased import", "from myapp.models import Book as Novel\nNovel.objects.filter(au|)"},
		{"qualified import", "import myapp.models as models\nmodels.Book.objects.filter(au|)"},
		{"standard qualified import", "import myapp.models\nmyapp.models.Book.objects.filter(au|)"},
		{"queryset assignment", "from myapp.models import Book\nqs = Book.objects.all()\nqs.filter(au|)"},
		{"nested multiline", "from myapp.models import Book\nBook.objects.filter(\n    title=func(value),\n    au|\n)"},
		{"parenthesis string", "from myapp.models import Book\nBook.objects.filter(title=\"(\", au|)"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source, offset := sourceAtCursor(t, test.source)
			context, ok := Analyze(source, offset, graph)
			if !ok || context.Kind != ContextQueryKeyword || context.Value.CanonicalLabel != "myapp.Book" || context.Identifier != "au" {
				t.Fatalf("Analyze() = %#v, %v", context, ok)
			}
			if string(source[context.Replacement.Start:context.Replacement.End]) != "au" {
				t.Fatalf("replacement = %q", source[context.Replacement.Start:context.Replacement.End])
			}
		})
	}
}

func TestAnalyzeModelAndInstanceMembers(t *testing.T) {
	graph := inferenceTestGraph(t)
	tests := []struct {
		name   string
		source string
		kind   ContextKind
	}{
		{"model root", "from myapp.models import Book\nBook.au|", ContextModelMember},
		{"constructor", "from myapp.models import Book\nbook = Book()\nbook.au|", ContextInstanceMember},
		{"annotation", "from myapp.models import Book\nbook: Book\nbook.au|", ContextInstanceMember},
		{"assignment", "from myapp.models import Book\nbook: Book\nother = book\nother.au|", ContextInstanceMember},
		{"get return", "from myapp.models import Book\nbook = Book.objects.get(id=1)\nbook.au|", ContextInstanceMember},
		{"first return", "from myapp.models import Book\nbook = Book.objects.first()\nbook.au|", ContextInstanceMember},
		{"spaced constructor", "from myapp.models import Book\nbook = Book ()\nbook.au|", ContextInstanceMember},
		{"parenthesis string return", "from myapp.models import Book\nbook = Book.objects.get (title=\"(\")\nbook.au|", ContextInstanceMember},
		{"Unicode field", "from myapp.models import Book\nbook = Book()\nbook.café|", ContextInstanceMember},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source, offset := sourceAtCursor(t, test.source)
			context, ok := Analyze(source, offset, graph)
			if !ok || context.Kind != test.kind || context.Value.CanonicalLabel != "myapp.Book" {
				t.Fatalf("Analyze() = %#v, %v", context, ok)
			}
		})
	}
}

func TestAnalyzeRejectsUnknownAndInvalidContexts(t *testing.T) {
	graph := inferenceTestGraph(t)
	for _, sourceWithCursor := range []string{
		"unknown.au|",
		"from myapp.models import Book\nvalue = dynamic()\nvalue.au|",
		"from myapp.models import Book\nBook.objects.filter(title=au|)",
		"from myapp.models import Book\nBook.objects.filter(title=\"au|\")",
		"from myapp.models import Book\nbook = Book() if flag else object()\nbook.au|",
		"from myapp.models import Book\nbook = Book + 1\nbook.au|",
		"from myapp.models import Book\nbooks: list[Book]\nbooks.filter(au|)",
		"from myapp.models import Book\ntext = \"Book.objects.filter(author|)\"",
		"from myapp.models import Book\n# Book.objects.filter(author|)",
	} {
		source, offset := sourceAtCursor(t, sourceWithCursor)
		if context, ok := Analyze(source, offset, graph); ok {
			t.Fatalf("Analyze(%q) = %#v, true", sourceWithCursor, context)
		}
	}
}

func TestAnalyzeSyntaxExcludesClassBindings(t *testing.T) {
	graph := inferenceTestGraph(t)
	store := newTestStore(t)
	uri := "file:///class-scope.py"
	sourceWithCursor := "class Holder:\n    from myapp.models import Book\n\nBook.objects.filter(au|)"
	source, offset := sourceAtCursor(t, sourceWithCursor)
	if err := store.Open(uri, 1, string(source)); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := store.Snapshot(uri)
	if context, ok := AnalyzeSyntax(snapshot.Source, offset, graph, snapshot.Syntax); ok {
		t.Fatalf("class-local import inferred as %#v; syntax=%#v", context, snapshot.Syntax)
	}
}

func TestAnalyzeSyntaxInfersAnnotatedParameter(t *testing.T) {
	graph := inferenceTestGraph(t)
	store := newTestStore(t)
	uri := "file:///parameter.py"
	sourceWithCursor := "from myapp.models import Book\n\ndef handle(book: Book = None):\n    return book.au|"
	source, offset := sourceAtCursor(t, sourceWithCursor)
	if err := store.Open(uri, 1, string(source)); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := store.Snapshot(uri)
	context, ok := AnalyzeSyntax(snapshot.Source, offset, graph, snapshot.Syntax)
	if !ok || context.Kind != ContextInstanceMember || context.Value.CanonicalLabel != "myapp.Book" {
		t.Fatalf("annotated parameter AnalyzeSyntax() = %#v, %v; syntax=%#v", context, ok, snapshot.Syntax)
	}
}

func TestAnalyzeSyntaxFileTraversesSingleValuedRelationsFromModelSelf(t *testing.T) {
	graph := inferenceTestGraph(t)
	tests := []struct {
		name   string
		source string
		model  string
	}{
		{"forward relation", "class Book:\n    def author_name(self):\n        return self.author.na|", "myapp.Author"},
		{"reverse one-to-one chain", "class Book:\n    def profile_bio(self):\n        return self.author.profile.bi|", "myapp.Profile"},
		{"annotated instance chain", "from myapp.models import Book\nbook: Book\nbook.author.profile.bi|", "myapp.Profile"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := newTestStore(t)
			source, offset := sourceAtCursor(t, test.source)
			if err := store.Open("file:///project/myapp/models.py", 1, string(source)); err != nil {
				t.Fatal(err)
			}
			snapshot, _ := store.Snapshot("file:///project/myapp/models.py")
			context, ok := AnalyzeSyntaxFile(snapshot.Source, offset, graph, snapshot.Syntax, inferenceTestModelPath())
			if !ok || context.Kind != ContextInstanceMember || context.Value.CanonicalLabel != test.model {
				t.Fatalf("AnalyzeSyntaxFile() = %#v, %v; syntax=%#v", context, ok, snapshot.Syntax)
			}
		})
	}
}

func TestAnalyzeSyntaxFileDoesNotTreatRelatedManagersAsInstances(t *testing.T) {
	graph := inferenceTestGraph(t)
	for _, sourceWithCursor := range []string{
		"class Author:\n    def title(self):\n        return self.books.ti|",
		"class Book:\n    def invalid(self):\n        self = object()\n        return self.author.na|",
		"class Book:\n    def invalid(other, self):\n        return self.author.na|",
		"class Book:\n    @staticmethod\n    def invalid(self):\n        return self.author.na|",
		"class Book:\n    @classmethod\n    def invalid(self):\n        return self.author.na|",
	} {
		store := newTestStore(t)
		source, offset := sourceAtCursor(t, sourceWithCursor)
		if err := store.Open("file:///project/myapp/models.py", 1, string(source)); err != nil {
			t.Fatal(err)
		}
		snapshot, _ := store.Snapshot("file:///project/myapp/models.py")
		if context, ok := AnalyzeSyntaxFile(snapshot.Source, offset, graph, snapshot.Syntax, inferenceTestModelPath()); ok {
			t.Fatalf("unsafe relation inference = %#v", context)
		}
	}
}

func TestAnalyzeSyntaxFileUsesNestedModelQualname(t *testing.T) {
	graph := inferenceTestGraph(t)
	store := newTestStore(t)
	source, offset := sourceAtCursor(t, "class Namespace:\n    class Nested:\n        def value(self):\n            return self.na|")
	if err := store.Open("file:///project/myapp/models.py", 1, string(source)); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := store.Snapshot("file:///project/myapp/models.py")
	context, ok := AnalyzeSyntaxFile(snapshot.Source, offset, graph, snapshot.Syntax, inferenceTestModelPath())
	if !ok || context.Kind != ContextInstanceMember || context.Value.CanonicalLabel != "myapp.Nested" {
		t.Fatalf("nested AnalyzeSyntaxFile() = %#v, %v", context, ok)
	}
}

func TestAnalyzeSyntaxParameterDoesNotOverrideLaterAssignment(t *testing.T) {
	graph := inferenceTestGraph(t)
	for _, sourceWithCursor := range []string{
		"from myapp.models import Book\n\ndef handle(book: Book):\n    book = object()\n    return book.au|",
		"from myapp.models import Book\n\ndef handle(book=Book()):\n    return book.au|",
	} {
		context, ok := analyzeStoredSource(t, graph, sourceWithCursor)
		if ok {
			t.Fatalf("unsafe parameter inference = %#v", context)
		}
	}
}

func TestAnalyzeSyntaxClassBodyAndQuerySetAlias(t *testing.T) {
	graph := inferenceTestGraph(t)
	tests := []string{
		"class Holder:\n    from myapp.models import Book\n    book = Book()\n    book.au|",
		"from django.db.models import QuerySet as QS\nfrom myapp.models import Book\nbooks: QS[Book]\nbooks.filter(au|)",
	}
	for _, sourceWithCursor := range tests {
		context, ok := analyzeStoredSource(t, graph, sourceWithCursor)
		if !ok || context.Value.CanonicalLabel != "myapp.Book" {
			t.Fatalf("AnalyzeSyntax(%q) = %#v, %v", sourceWithCursor, context, ok)
		}
	}
}

func TestAnalyzeSyntaxRejectsGuardedAssignments(t *testing.T) {
	graph := inferenceTestGraph(t)
	for _, sourceWithCursor := range []string{
		"from myapp.models import Book\nif condition:\n    book = Book()\nbook.au|",
		"from myapp.models import Book\nfor item in values:\n    book = Book()\nbook.au|",
		"from myapp.models import Book\ntry:\n    book = Book()\nexcept Exception:\n    pass\nbook.au|",
	} {
		if context, ok := analyzeStoredSource(t, graph, sourceWithCursor); ok {
			t.Fatalf("guarded assignment inferred as %#v", context)
		}
	}
}

func analyzeStoredSource(t *testing.T, graph *schema.Graph, sourceWithCursor string) (Context, bool) {
	t.Helper()
	store := newTestStore(t)
	uri := "file:///stored.py"
	source, offset := sourceAtCursor(t, sourceWithCursor)
	if err := store.Open(uri, 1, string(source)); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := store.Snapshot(uri)
	return AnalyzeSyntax(snapshot.Source, offset, graph, snapshot.Syntax)
}

func TestAnalyzeSyntaxKeepsFunctionLocalsBounded(t *testing.T) {
	graph := inferenceTestGraph(t)
	store := newTestStore(t)
	uri := "file:///scope.py"
	sourceWithCursor := "from myapp.models import Book\n\ndef helper():\n    value = Book()\n\nvalue.au|"
	source, offset := sourceAtCursor(t, sourceWithCursor)
	if err := store.Open(uri, 1, string(source)); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := store.Snapshot(uri)
	if context, ok := AnalyzeSyntax(snapshot.Source, offset, graph, snapshot.Syntax); ok {
		t.Fatalf("out-of-scope assignment inferred as %#v", context)
	}
}

func TestAnalyzeSyntaxSupportsParenthesizedMultilineAssignment(t *testing.T) {
	graph := inferenceTestGraph(t)
	store := newTestStore(t)
	uri := "file:///multiline.py"
	sourceWithCursor := "from myapp.models import Book\nbook = (\n    Book.objects.first()\n)\nbook.au|"
	source, offset := sourceAtCursor(t, sourceWithCursor)
	if err := store.Open(uri, 1, string(source)); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := store.Snapshot(uri)
	context, ok := AnalyzeSyntax(snapshot.Source, offset, graph, snapshot.Syntax)
	if !ok || context.Kind != ContextInstanceMember || context.Value.CanonicalLabel != "myapp.Book" {
		t.Fatalf("multiline assignment AnalyzeSyntax() = %#v, %v", context, ok)
	}
}

func sourceAtCursor(t *testing.T, value string) ([]byte, int) {
	t.Helper()
	if strings.Count(value, "|") != 1 {
		t.Fatalf("source must contain one cursor: %q", value)
	}
	offset := strings.IndexByte(value, '|')
	return []byte(value[:offset] + value[offset+1:]), offset
}

func inferenceTestGraph(t *testing.T) *schema.Graph {
	t.Helper()
	authorLabel := "myapp.Author"
	bookLabel := "myapp.Book"
	profileLabel := "myapp.Profile"
	attname := "author_id"
	forward := "forward"
	reverse := "reverse"
	manyToOne := "many-to-one"
	oneToMany := "one-to-many"
	oneToOne := "one-to-one"
	profileAccessor := "profile"
	profileQuery := "profile"
	booksAccessor := "books"
	bookQuery := "book"
	models := map[string]schema.Model{
		"Author": testModel("Author", map[string]schema.Field{
			"id":   testField("myapp.Author", "id", "django.db.models.fields.BigAutoField"),
			"name": testField("myapp.Author", "name", "django.db.models.fields.CharField"),
			"profile": {
				Type: "django.db.models.fields.reverse_related.OneToOneRel", InternalType: "OneToOneRel", Name: "profile",
				IsRelation: true, RelatedModel: &profileLabel, SourceModel: "myapp.Profile", SourceRange: testSourceRangeAt(20),
				RelationDirection: &reverse, RelationCardinality: &oneToOne, AccessorName: &profileAccessor, QueryName: &profileQuery,
			},
			"book": {
				Type: "django.db.models.fields.reverse_related.ManyToOneRel", InternalType: "ManyToOneRel", Name: "book",
				IsRelation: true, RelatedModel: &bookLabel, SourceModel: "myapp.Book", SourceRange: testSourceRange(),
				RelationDirection: &reverse, RelationCardinality: &oneToMany, AccessorName: &booksAccessor, QueryName: &bookQuery,
			},
		}),
		"Book": testModel("Book", map[string]schema.Field{
			"id":    testField("myapp.Book", "id", "django.db.models.fields.BigAutoField"),
			"title": testField("myapp.Book", "title", "django.db.models.fields.CharField"),
			"café":  testField("myapp.Book", "café", "django.db.models.fields.CharField"),
			"author": {
				Type:                "django.db.models.fields.related.ForeignKey",
				InternalType:        "ForeignKey",
				Name:                "author",
				Attname:             &attname,
				IsRelation:          true,
				RelatedModel:        &authorLabel,
				SourceModel:         "myapp.Book",
				SourceRange:         testSourceRangeAt(10),
				RelationDirection:   &forward,
				RelationCardinality: &manyToOne,
			},
		}),
		"Profile": testModel("Profile", map[string]schema.Field{
			"id": testField("myapp.Profile", "id", "django.db.models.fields.BigAutoField"),
			"bio": func() schema.Field {
				field := testField("myapp.Profile", "bio", "django.db.models.fields.TextField")
				field.SourceRange = testSourceRangeAt(30)
				return field
			}(),
		}),
	}
	nested := testModel("Nested", map[string]schema.Field{
		"name": testField("myapp.Nested", "name", "django.db.models.fields.CharField"),
	})
	nested.Qualname = "Namespace.Nested"
	models["Nested"] = nested
	graph, err := schema.Build(schema.Snapshot{
		SchemaVersion:           schema.Version,
		PositionEncoding:        "utf-8-bytes",
		LookupTransformMaxDepth: 2,
		LookupPathMaxCount:      512,
		Apps: map[string]schema.App{
			"myapp": {Label: "myapp", ImportName: "myapp", RootPath: filepath.Dir(inferenceTestModelPath()), Models: models},
		},
	})
	if err != nil {
		t.Fatalf("schema.Build() error = %v", err)
	}
	return graph
}

func testModel(name string, fields map[string]schema.Field) schema.Model {
	return schema.Model{
		CanonicalLabel: "myapp." + name,
		Module:         "myapp.models",
		Qualname:       name,
		FilePath:       inferenceTestModelPath(),
		LineNumber:     1,
		SourceRange:    testSourceRange(),
		Managed:        true,
		DefaultManager: "objects",
		BaseManager:    schema.BaseManager{Name: "_base_manager", OwnerClass: "django.db.models.Manager"},
		Managers: []schema.Manager{{
			Name: "objects", OwnerClass: "django.db.models.Manager", SourceRange: testSourceRange(),
		}},
		Fields: fields,
	}
}

func testField(sourceModel, name, fieldType string) schema.Field {
	return schema.Field{
		Type:         fieldType,
		InternalType: schema.ModelName(fieldType),
		Name:         name,
		SourceModel:  sourceModel,
		SourceRange:  testSourceRange(),
	}
}

func testSourceRange() *schema.SourceRange {
	return testSourceRangeAt(1)
}

func testSourceRangeAt(line int) *schema.SourceRange {
	return &schema.SourceRange{
		FilePath:     inferenceTestModelPath(),
		SourceDigest: strings.Repeat("0", 64),
		Start:        schema.Position{Line: line},
		End:          schema.Position{Line: line, Column: 1},
	}
}

func inferenceTestModelPath() string {
	return filepath.Join(os.TempDir(), "pogo-tests", "project", "myapp", "models.py")
}
