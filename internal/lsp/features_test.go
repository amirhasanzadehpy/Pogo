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

func TestAnnotateAliasCompletionAndHover(t *testing.T) {
	features := testFeatures(t)
	defer features.Close()
	uri := "file:///annotate.py"
	source := "from myapp.models import Author\nauthor = Author.objects.annotate(has_books=Exists(p)).first()\nauthor.has_books\n"
	if err := features.documents.Open(uri, 1, source); err != nil {
		t.Fatal(err)
	}

	completion, err := features.Completion(uri, analysis.Position{Line: 2, Character: 7})
	if err != nil {
		t.Fatalf("Completion() error = %v", err)
	}
	if completion == nil {
		t.Fatal("Completion() = nil, want alias in results")
	}
	found := false
	for _, item := range completion.Items {
		if item.Label == "has_books" {
			found = true
			if item.Detail == nil || *item.Detail != "QuerySet annotation" {
				t.Errorf("completion detail = %v, want 'QuerySet annotation'", item.Detail)
			}
		}
	}
	if !found {
		t.Errorf("completion items = %v, want has_books", completion.Items)
	}

	hover, err := features.Hover(uri, analysis.Position{Line: 2, Character: 10})
	if err != nil {
		t.Fatalf("Hover() error = %v", err)
	}
	if hover == nil {
		t.Fatal("Hover() = nil, want annotation hover")
	}
	markup, ok := hover.Contents.(protocol.MarkupContent)
	if !ok || !strings.Contains(markup.Value, "**has\\_books**") || !strings.Contains(markup.Value, "QuerySet annotation") {
		t.Errorf("hover contents = %#v", hover.Contents)
	}
}

func TestAnnotateAliasORMPathCompletionAndHover(t *testing.T) {
	features := testFeatures(t)
	defer features.Close()
	uri := "file:///annotate-path.py"
	source := "from myapp.models import Author\nAuthor.objects.annotate(count=Count(\"id\")).values_list(\"pk\", \"count\")\n"
	if err := features.documents.Open(uri, 1, source); err != nil {
		t.Fatal(err)
	}

	completion, err := features.Completion(uri, analysis.Position{Line: 1, Character: 64})
	if err != nil {
		t.Fatalf("Completion() error = %v", err)
	}
	if completion == nil {
		t.Fatal("Completion() = nil, want alias in results")
	}
	found := false
	for _, item := range completion.Items {
		if item.Label == "count" {
			found = true
		}
	}
	if !found {
		t.Errorf("completion items = %v, want count", completion.Items)
	}

	hover, err := features.Hover(uri, analysis.Position{Line: 1, Character: 65})
	if err != nil {
		t.Fatalf("Hover() error = %v", err)
	}
	if hover == nil {
		t.Fatal("Hover() = nil, want annotation hover")
	}
	markup, ok := hover.Contents.(protocol.MarkupContent)
	if !ok || !strings.Contains(markup.Value, "**count**") || !strings.Contains(markup.Value, "QuerySet annotation") || !strings.Contains(markup.Value, "Return type") || !strings.Contains(markup.Value, "`int`") {
		t.Errorf("hover contents = %#v", hover.Contents)
	}
}

func TestAnnotateAliasBareKeywordHoverShowsReturnType(t *testing.T) {
	features := testFeatures(t)
	defer features.Close()
	uri := "file:///annotate-keyword.py"
	source := "from myapp.models import Author\nAuthor.objects.annotate(has_books=Exists(x)).filter(has_books=True)\n"
	if err := features.documents.Open(uri, 1, source); err != nil {
		t.Fatal(err)
	}
	hover, err := features.Hover(uri, analysis.Position{Line: 1, Character: 56})
	if err != nil {
		t.Fatalf("Hover() error = %v", err)
	}
	if hover == nil {
		t.Fatal("Hover() = nil, want annotation hover")
	}
	markup, ok := hover.Contents.(protocol.MarkupContent)
	if !ok || !strings.Contains(markup.Value, "**has\\_books**") || !strings.Contains(markup.Value, "`bool`") {
		t.Errorf("hover contents = %#v", hover.Contents)
	}
}

func TestAnnotateAliasInsideFExpressionHover(t *testing.T) {
	features := testFeatures(t)
	defer features.Close()
	uri := "file:///annotate-f-expr.py"
	source := "from django.db.models import F, Count\nfrom myapp.models import Author\nAuthor.objects.annotate(user_count=Count(\"id\")).annotate(total=F(\"user_count\") + 1)\n"
	if err := features.documents.Open(uri, 1, source); err != nil {
		t.Fatal(err)
	}

	completion, err := features.Completion(uri, analysis.Position{Line: 2, Character: 70})
	if err != nil {
		t.Fatalf("Completion() error = %v", err)
	}
	if completion == nil {
		t.Fatal("Completion() = nil, want alias in results")
	}
	found := false
	for _, item := range completion.Items {
		if item.Label == "user_count" {
			found = true
		}
	}
	if !found {
		t.Errorf("completion items = %v, want user_count", completion.Items)
	}

	hover, err := features.Hover(uri, analysis.Position{Line: 2, Character: 70})
	if err != nil {
		t.Fatalf("Hover() error = %v", err)
	}
	if hover == nil {
		t.Fatal("Hover() = nil, want annotation hover")
	}
	markup, ok := hover.Contents.(protocol.MarkupContent)
	if !ok || !strings.Contains(markup.Value, "**user\\_count**") || !strings.Contains(markup.Value, "QuerySet annotation") || !strings.Contains(markup.Value, "`int`") {
		t.Errorf("hover contents = %#v", hover.Contents)
	}

	location, err := features.Definition(uri, analysis.Position{Line: 2, Character: 70})
	if err != nil || location == nil {
		t.Fatalf("Definition() = %#v, %v", location, err)
	}
	if string(location.URI) != uri {
		t.Fatalf("Definition() URI = %s, want %s", location.URI, uri)
	}
	snapshot, _ := features.documents.Snapshot(uri)
	start, startOK := analysis.ByteOffset(snapshot.Source, analysis.Position{Line: uint32(location.Range.Start.Line), Character: uint32(location.Range.Start.Character)})
	end, endOK := analysis.ByteOffset(snapshot.Source, analysis.Position{Line: uint32(location.Range.End.Line), Character: uint32(location.Range.End.Character)})
	if !startOK || !endOK || string(snapshot.Source[start:end]) != "user_count" || end >= len(snapshot.Source) || snapshot.Source[end] != '=' {
		t.Fatalf("Definition() range = %#v, text %q", location.Range, snapshot.Source[start:min(end+1, len(snapshot.Source))])
	}
}

func TestModelSelfRelationChainCompletionAndHover(t *testing.T) {
	features := testFeatures(t)
	defer features.Close()
	uri := mustSourceFileURI(t, featureTestModelPath())
	completionSource, completionPosition := lspSourceAtCursor(t, "class Book:\n    def profile_name(self):\n        return self.author.profile.dis|")
	if err := features.documents.Open(uri, 1, string(completionSource)); err != nil {
		t.Fatal(err)
	}
	completion, err := features.Completion(uri, completionPosition)
	if err != nil || completion == nil || len(completion.Items) != 1 || completion.Items[0].Label != "display_name" {
		t.Fatalf("Completion() = %#v, %v", completion, err)
	}

	hoverSource, hoverPosition := lspSourceAtCursor(t, "class Book:\n    def profile_name(self):\n        return self.author.profile.display_na|me")
	if err := features.documents.Change(uri, 2, []analysis.Change{{Text: string(hoverSource)}}); err != nil {
		t.Fatal(err)
	}
	hover, err := features.Hover(uri, hoverPosition)
	if err != nil || hover == nil {
		t.Fatalf("Hover() = %#v, %v", hover, err)
	}
	markup, ok := hover.Contents.(protocol.MarkupContent)
	if !ok || !strings.Contains(markup.Value, "**display\\_name**") {
		t.Fatalf("hover contents = %#v", hover.Contents)
	}
}

func TestModelSelfInferenceUsesFilePathResolvedAtOpen(t *testing.T) {
	root := t.TempDir()
	realPath := filepath.Join(root, "models.py")
	aliasPath := filepath.Join(root, "models-link.py")
	if err := os.WriteFile(realPath, []byte("# model source\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realPath, aliasPath); err != nil {
		t.Skipf("symlinks are unavailable: %v", err)
	}
	model := featureTestModel("Book", map[string]schema.Field{
		"title": featureTestField("myapp.Book", "title", "django.db.models.fields.CharField"),
	})
	model.FilePath = realPath
	modelRange := *model.SourceRange
	modelRange.FilePath = realPath
	model.SourceRange = &modelRange
	graph, err := schema.Build(schema.Snapshot{
		SchemaVersion: schema.Version, PositionEncoding: "utf-8-bytes", LookupTransformMaxDepth: 2, LookupPathMaxCount: 512,
		Apps: map[string]schema.App{"myapp": {
			Label: "myapp", ImportName: "myapp", RootPath: root, Models: map[string]schema.Model{"Book": model},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	cache := &schema.Cache{}
	cache.Replace(graph)
	features, err := NewFeatures(cache)
	if err != nil {
		t.Fatal(err)
	}
	defer features.Close()
	uri := mustSourceFileURI(t, aliasPath)
	source, position := lspSourceAtCursor(t, "class Book:\n    def title(self):\n        return self.ti|")
	if err := features.didOpen(nil, &protocol.DidOpenTextDocumentParams{TextDocument: protocol.TextDocumentItem{
		URI: protocol.DocumentUri(uri), LanguageID: "python", Version: 1, Text: string(source),
	}}); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := features.documents.Snapshot(uri)
	resolvedPath, err := filepath.EvalSymlinks(realPath)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.FilePath != resolvedPath {
		t.Fatalf("cached file path = %q, want %q", snapshot.FilePath, resolvedPath)
	}
	completion, err := features.Completion(uri, position)
	if err != nil || completion == nil || len(completion.Items) != 1 || completion.Items[0].Label != "title" {
		t.Fatalf("Completion() through symlink = %#v, %v", completion, err)
	}
}

func TestDidOpenDoesNotCacheInvalidFilePath(t *testing.T) {
	features, err := NewFeatures(&schema.Cache{})
	if err != nil {
		t.Fatal(err)
	}
	defer features.Close()
	if err := features.didOpen(nil, &protocol.DidOpenTextDocumentParams{TextDocument: protocol.TextDocumentItem{
		URI: "file://", LanguageID: "python", Version: 1, Text: "value = 1\n",
	}}); err != nil {
		t.Fatal(err)
	}
	snapshot, ok := features.documents.Snapshot("file://")
	if !ok || snapshot.FilePath != "" {
		t.Fatalf("invalid URI cached file path %q", snapshot.FilePath)
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
		{"Q lookup relation", "from django.db.models import Q\nfrom myapp.models import Book\nBook.objects.filter(Q(author__na|=value))", []string{"name"}},
		{"chained lookup relation after dunder", "from myapp.models import Book\nBook.objects.exclude(title=\"\").exclude(author__|)", []string{"book", "id", "name", "profile", "exact", "in", "isnull"}},
		{"lookup transform", "from myapp.models import Book\nBook.objects.filter(published_at__date__g|=value)", []string{"gte"}},
		{"projection", "from myapp.models import Book\nBook.objects.values(\"title\", \"author__na|\")", []string{"name"}},
		{"only", "from myapp.models import Book\nBook.objects.only(\"author__na|\")", []string{"name"}},
		{"select related", "from myapp.models import Book\nBook.objects.select_related(\"author__pro|\")", []string{"profile"}},
		{"annotated queryset select related", "from django.db.models import QuerySet\nfrom myapp.models import Book\nitems: QuerySet[Book] = get_items()\nitems.select_related(\"author__pro|\")", []string{"profile"}},
		{"annotated queryset prefetch related", "from django.db.models import QuerySet\nfrom myapp.models import Book\nitems: QuerySet[Book] = get_items()\nitems.prefetch_related(\"author__pro|\")", []string{"profile"}},
		{"prefetch related", "from myapp.models import Book\nBook.objects.prefetch_related(\"sto|\")", []string{"store_set"}},
		{"prefetch related after dunder", "from myapp.models import Book\nBook.objects.prefetch_related(\"author__|\")", []string{"books", "profile"}},
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

func TestCompletionFromEnclosingModelClassName(t *testing.T) {
	features := testFeatures(t)
	defer features.Close()
	uri := mustSourceFileURI(t, featureTestModelPath())
	source, position := lspSourceAtCursor(t, "class Book:\n    @staticmethod\n    def update():\n        Book.objects.exclude(title=\"\").exclude(author__|)")
	if err := features.documents.Open(uri, 1, string(source)); err != nil {
		t.Fatal(err)
	}
	completion, err := features.Completion(uri, position)
	if err != nil || completion == nil {
		t.Fatalf("Completion() = %#v, %v", completion, err)
	}
	found := false
	for _, item := range completion.Items {
		if item.Label == "name" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("completion items = %#v, want related model fields", completion.Items)
	}
}

func TestMetaFieldCompletion(t *testing.T) {
	features := testFeatures(t)
	defer features.Close()
	for index, sourceWithCursor := range []string{
		"class Book:\n    class Meta:\n        constraints = [models.UniqueConstraint(fields=['ti|'], name='unique_book_per_author')]",
		"class Book:\n    class Meta:\n        ordering = ['-ti|']",
		"class Book:\n    class Meta:\n        unique_together = [('ti|', 'author')]",
	} {
		source, position := lspSourceAtCursor(t, sourceWithCursor)
		documentURI := fmt.Sprintf("file:///meta-%d.py", index)
		if err := features.documents.OpenFile(documentURI, featureTestModelPath(), 1, string(source)); err != nil {
			t.Fatal(err)
		}
		completion, err := features.Completion(documentURI, position)
		if err != nil || completion == nil || len(completion.Items) != 1 || completion.Items[0].Label != "title" {
			t.Fatalf("Completion() = %#v, %v", completion, err)
		}
		edit := completion.Items[0].TextEdit.(protocol.TextEdit)
		if edit.NewText != "title" {
			t.Fatalf("text edit = %#v", edit)
		}
	}
}

func TestMetaFieldHover(t *testing.T) {
	features := testFeatures(t)
	defer features.Close()
	uri := "file:///meta-hover.py"
	source, position := lspSourceAtCursor(t, "class Book:\n    class Meta:\n        ordering = ['ti|tle']")
	if err := features.documents.OpenFile(uri, featureTestModelPath(), 1, string(source)); err != nil {
		t.Fatal(err)
	}
	hover, err := features.Hover(uri, position)
	if err != nil || hover == nil {
		t.Fatalf("Hover() = %#v, %v", hover, err)
	}
	markup, ok := hover.Contents.(protocol.MarkupContent)
	if !ok || !strings.Contains(markup.Value, "**title**") || !strings.Contains(markup.Value, "CharField") {
		t.Fatalf("hover contents = %#v", hover.Contents)
	}
}

func TestCompletionInsideNestedQLookup(t *testing.T) {
	features := testFeatures(t)
	defer features.Close()
	uri := "file:///nested-q.py"
	source := []byte("from django.db.models import Count, Q\nfrom myapp.models import Book\nBook.objects.annotate(total=Count(\"id\")).filter(Q(id__in=ids) | Q(author__name=value))")
	offset := strings.Index(string(source), "author__na") + len("author__na")
	position, ok := analysis.PositionAt(source, offset)
	if !ok {
		t.Fatal("cursor position is invalid")
	}
	if err := features.documents.Open(uri, 1, string(source)); err != nil {
		t.Fatal(err)
	}
	completion, err := features.Completion(uri, position)
	if err != nil || completion == nil {
		t.Fatalf("Completion() = %#v, %v", completion, err)
	}
	for _, item := range completion.Items {
		if item.Label == "name" {
			hoverOffset := strings.Index(string(source), "author__name") + len("author__name")
			hoverPosition, valid := analysis.PositionAt(source, hoverOffset)
			if !valid {
				t.Fatal("hover position is invalid")
			}
			hover, hoverErr := features.Hover(uri, hoverPosition)
			if hoverErr != nil || hover == nil {
				t.Fatalf("Hover() = %#v, %v", hover, hoverErr)
			}
			markup, ok := hover.Contents.(protocol.MarkupContent)
			if !ok || !strings.Contains(markup.Value, "name") {
				t.Fatalf("hover contents = %#v", hover.Contents)
			}
			return
		}
	}
	t.Fatalf("completion items = %#v, want related model fields", completion.Items)
}

func TestDjangoShortcutReverseManagerPathFeatures(t *testing.T) {
	features := testFeatures(t)
	defer features.Close()
	uri := "file:///shortcut-reverse-manager.py"
	source, position := lspSourceAtCursor(t, "from django.db.models import Q\nfrom django.shortcuts import get_object_or_404\nfrom myapp.models import Book\nbook = get_object_or_404(Book, id=1)\nbook.author.books.filter(Q(author__na|me=value))")
	if err := features.documents.Open(uri, 1, string(source)); err != nil {
		t.Fatal(err)
	}
	completion, err := features.Completion(uri, position)
	if err != nil || completion == nil {
		t.Fatalf("Completion() = %#v, %v", completion, err)
	}
	found := false
	for _, item := range completion.Items {
		if item.Label == "name" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("completion items = %#v, want related model fields", completion.Items)
	}
	hover, err := features.Hover(uri, position)
	if err != nil || hover == nil {
		t.Fatalf("Hover() = %#v, %v", hover, err)
	}
}

func TestDjangoFunctionExpressionPathFeatures(t *testing.T) {
	features := testFeatures(t)
	defer features.Close()
	uri := "file:///expression-path.py"
	source, position := lspSourceAtCursor(t, "from django.db.models import Value\nfrom django.db.models.functions import Concat\nfrom myapp.models import Book\nBook.objects.annotate(label=Concat(\"author__na|me\", Value(\" \")))")
	if err := features.documents.Open(uri, 1, string(source)); err != nil {
		t.Fatal(err)
	}
	completion, err := features.Completion(uri, position)
	if err != nil || completion == nil {
		t.Fatalf("Completion() = %#v, %v", completion, err)
	}
	found := false
	for _, item := range completion.Items {
		if item.Label == "name" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("completion items = %#v, want related model fields", completion.Items)
	}
	hover, err := features.Hover(uri, position)
	if err != nil || hover == nil {
		t.Fatalf("Hover() = %#v, %v", hover, err)
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
	manager.Start(ctx, func(_ uint64, err error) {
		if err != nil {
			failure <- err
		}
	})
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
	definitionOffset := strings.Index(string(source), "author__") + 2
	definitionPosition, _ := analysis.PositionAt(source, definitionOffset)
	if _, err := features.Definition(uri, definitionPosition); err != nil {
		t.Fatalf("Definition() with stopped worker error = %v", err)
	}
	if got := manager.RequestCount(); got != requestsAfterStop {
		t.Fatalf("worker requests after cached feature handlers = %d, want %d", got, requestsAfterStop)
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
	b.ReportMetric(float64(percentile(totals, 99).Nanoseconds())/1000, "p99-us")
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

func BenchmarkHoverHandler(b *testing.B) {
	features := testFeatures(b)
	defer features.Close()
	uri := "file:///hover-handler-benchmark.py"
	if err := features.documents.Open(uri, 1, "from myapp.models import Book\nBook.objects.filter(author__name=1)\n"); err != nil {
		b.Fatal(err)
	}
	benchmarkNavigationLatency(b, func() {
		hover, err := features.Hover(uri, analysis.Position{Line: 1, Character: 30})
		if err != nil || hover == nil {
			b.Fatalf("Hover() = %#v, %v", hover, err)
		}
	})
}

func BenchmarkDiagnosticLatency(b *testing.B) {
	features := testFeatures(b)
	defer features.Close()
	features.SetNotifier(func(string, any) {})
	uri := "file:///diagnostic-benchmark.py"
	if err := features.documents.Open(uri, 1, "from myapp.models import Book\nBook.objects.filter(titel=1)\n"); err != nil {
		b.Fatal(err)
	}
	version := int32(1)
	totals := make([]time.Duration, b.N)
	parseTotal := time.Duration(0)
	diagnosticTotal := time.Duration(0)
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		started := time.Now()
		version++
		text := "title"
		if index%2 == 1 {
			text = "titel"
		}
		parseStarted := time.Now()
		if err := features.documents.Change(uri, version, []analysis.Change{{
			Range: &analysis.Range{Start: analysis.Position{Line: 1, Character: 20}, End: analysis.Position{Line: 1, Character: 25}},
			Text:  text,
		}}); err != nil {
			b.Fatal(err)
		}
		parseTotal += time.Since(parseStarted)
		diagnosticStarted := time.Now()
		features.publishURI(uri)
		diagnosticTotal += time.Since(diagnosticStarted)
		totals[index] = time.Since(started)
	}
	b.StopTimer()
	sort.Slice(totals, func(left, right int) bool { return totals[left] < totals[right] })
	b.ReportMetric(float64(parseTotal.Nanoseconds())/float64(b.N), "parse-ns/op")
	b.ReportMetric(float64(diagnosticTotal.Nanoseconds())/float64(b.N), "diagnostic-ns/op")
	b.ReportMetric(float64(percentile(totals, 50).Nanoseconds())/1000, "p50-us")
	p95 := percentile(totals, 95)
	b.ReportMetric(float64(p95.Nanoseconds())/1000, "p95-us")
	b.ReportMetric(float64(percentile(totals, 99).Nanoseconds())/1000, "p99-us")
	if p95 >= 10*time.Millisecond {
		b.Fatalf("loaded-cache diagnostic p95 = %s, want < 10ms", p95)
	}
}

func BenchmarkDiagnosticHandler(b *testing.B) {
	features := testFeatures(b)
	defer features.Close()
	features.SetNotifier(func(string, any) {})
	uri := "file:///diagnostic-handler-benchmark.py"
	if err := features.documents.Open(uri, 1, "from myapp.models import Book\nBook.objects.filter(author__missing=1)\n"); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		features.publishURI(uri)
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
	bookLabel := "myapp.Book"
	storeLabel := "myapp.Store"
	profileName := "profile"
	bookQuery := "book"
	booksAccessor := "books"
	oneToMany := "one-to-many"
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
	activeMethod := schema.Method{Name: "active", OwnerClass: querySetClass, Signature: &activeSignature, Docstring: &activeDoc, Chainable: true, AssumedChainable: true, SourceRange: featureTestSourceRange()}
	activeKey := schema.MethodKey{Name: activeMethod.Name, OwnerClass: activeMethod.OwnerClass}
	graph, err := schema.Build(schema.Snapshot{
		SchemaVersion: schema.Version, PositionEncoding: "utf-8-bytes", LookupTransformMaxDepth: 2, LookupPathMaxCount: 512,
		QuerySetMethodDefs: []schema.Method{activeMethod},
		Apps: map[string]schema.App{"myapp": {
			Label: "myapp", ImportName: "myapp", RootPath: filepath.Dir(featureTestModelPath()),
			Models: map[string]schema.Model{
				"Author": featureTestModel("Author", map[string]schema.Field{
					"id":   featureTestField("myapp.Author", "id", "django.db.models.fields.BigAutoField"),
					"name": featureTestField("myapp.Author", "name", "django.db.models.fields.CharField"),
					"profile": {
						Type: "django.db.models.fields.reverse_related.OneToOneRel", InternalType: "OneToOneRel", Name: "profile",
						IsRelation: true, RelatedModel: &profileLabel, RelationDirection: &reverse, RelationCardinality: &oneToOne,
						QueryName: &profileName, AccessorName: &profileName, SourceModel: "myapp.Profile", SourceRange: featureTestSourceRange(),
					},
					"book": {
						Type: "django.db.models.fields.reverse_related.ManyToOneRel", InternalType: "ManyToOneRel", Name: "book",
						IsRelation: true, RelatedModel: &bookLabel, RelationDirection: &reverse, RelationCardinality: &oneToMany,
						QueryName: &bookQuery, AccessorName: &booksAccessor, SourceModel: "myapp.Book", SourceRange: featureTestSourceRange(),
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
						{Name: "objects", OwnerClass: "django.db.models.ManagerFromBookQuerySet", QuerySetClass: &querySetClass, SourceRange: featureTestSourceRange(), QuerySetMethods: []schema.BoundQuerySetMethod{{Method: activeKey, AvailableOnManager: true}}},
						{Name: "catalog", OwnerClass: "myapp.models.BookManager", SourceRange: featureTestSourceRange(), Methods: []schema.Method{{Name: "featured", OwnerClass: "myapp.models.BookManager", Signature: &featuredSignature, Chainable: true, SourceRange: featureTestSourceRange()}}},
					}
					model.QuerySetMethods = []schema.MethodKey{activeKey}
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
		FilePath: featureTestModelPath(), LineNumber: 1, SourceRange: featureTestSourceRange(), Managed: true,
		DefaultManager: "objects", BaseManager: schema.BaseManager{Name: "_base_manager", OwnerClass: "django.db.models.Manager"},
		Managers: []schema.Manager{{Name: "objects", OwnerClass: "django.db.models.Manager", SourceRange: featureTestSourceRange()}},
		Fields:   fields,
	}
}

func featureTestField(sourceModel, name, fieldType string) schema.Field {
	return schema.Field{Type: fieldType, InternalType: name, Name: name, SourceModel: sourceModel, SourceRange: featureTestSourceRange()}
}

func featureTestSourceRange() *schema.SourceRange {
	return &schema.SourceRange{FilePath: featureTestModelPath(), SourceDigest: strings.Repeat("0", 64), Start: schema.Position{Line: 1}, End: schema.Position{Line: 1, Column: 1}}
}

func featureTestModelPath() string {
	return filepath.Join(os.TempDir(), "pogo-tests", "project", "myapp", "models.py")
}

func mustSourceFileURI(t testingT, path string) string {
	t.Helper()
	uri, ok := sourceFileURI(path)
	if !ok {
		t.Fatalf("sourceFileURI(%q) failed", path)
	}
	return string(uri)
}

func lspSourceAtCursor(t testingT, value string) ([]byte, analysis.Position) {
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
