package analysis

import "testing"

func TestResolveDefinitionSyntaxAcrossORMReferences(t *testing.T) {
	graph := inferenceTestGraph(t)
	tests := []string{
		"from myapp.models import Bo|ok",
		"from myapp.models import Book as No|vel",
		"from myapp.models import Book as Novel\nNo|vel.objects.all()",
		"import myapp.models as models\nmodels.Bo|ok.objects.all()",
		"from myapp.models import Book\nBo|ok.objects.all()",
		"from myapp.models import Book\nBook.au|thor",
		"from myapp.models import Book\nbook = Book()\nbook.ti|tle",
		"from myapp.models import Book\nBook.objects.filter(au|thor__name=value)",
		"from myapp.models import Book\nBook.objects.filter(author__na|me=value)",
		"from myapp.models import Book\nBook.objects.values(\"author__na|me\")",
		"from myapp.models import Book\nBook.custom = value\nBo|ok.objects.all()",
	}
	for _, sourceWithCursor := range tests {
		t.Run(sourceWithCursor, func(t *testing.T) {
			source, offset := sourceAtCursor(t, sourceWithCursor)
			store, err := NewStore()
			if err != nil {
				t.Fatal(err)
			}
			defer store.CloseAll()
			if err := store.Open("file:///definition.py", 1, string(source)); err != nil {
				t.Fatal(err)
			}
			snapshot, _ := store.Snapshot("file:///definition.py")
			sourceRange, ok := ResolveDefinitionSyntax(source, offset, graph, snapshot.Syntax)
			if !ok || sourceRange.FilePath != inferenceTestModelPath() {
				t.Fatalf("ResolveDefinitionSyntax() = %#v, %v", sourceRange, ok)
			}
		})
	}
}

func TestResolveDefinitionSyntaxFileAcrossModelSelfRelationChain(t *testing.T) {
	graph := inferenceTestGraph(t)
	for _, test := range []struct {
		source string
		line   int
	}{
		{"class Book:\n    def profile(self):\n        return self.au|thor.profile.bio", 10},
		{"class Book:\n    def profile(self):\n        return self.author.pro|file.bio", 20},
		{"class Book:\n    def profile(self):\n        return self.author.profile.bi|o", 30},
	} {
		source, offset := sourceAtCursor(t, test.source)
		store, err := NewStore()
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Open("file:///project/myapp/models.py", 1, string(source)); err != nil {
			t.Fatal(err)
		}
		snapshot, _ := store.Snapshot("file:///project/myapp/models.py")
		sourceRange, ok := ResolveDefinitionSyntaxFile(source, offset, graph, snapshot.Syntax, inferenceTestModelPath())
		store.CloseAll()
		if !ok || sourceRange.FilePath != inferenceTestModelPath() || sourceRange.Start.Line != test.line {
			t.Fatalf("ResolveDefinitionSyntaxFile(%q) = %#v, %v", test.source, sourceRange, ok)
		}
	}
}

func TestResolveDefinitionSyntaxRejectsUnknownReferences(t *testing.T) {
	graph := inferenceTestGraph(t)
	for _, sourceWithCursor := range []string{
		"unknown.Bo|ok",
		"from myapp.models import Book\nBook.objects.filter(author_|_name=value)",
		"from myapp.models import Book\nBook.objects.filter(missing|=value)",
		"from myapp.models import Book\nBook = dynamic()\nBo|ok.objects.all()",
		"from myapp.models import Book\nif condition:\n    Book = dynamic()\nBo|ok.objects.all()",
		"from myapp.models import Book\ndef run(Book):\n    Bo|ok.objects.all()",
		"from myapp.models import Book\ndef run():\n    Bo|ok.objects.all()\n    Book = dynamic()",
		"from myapp.models import Book\ndef run():\n    Bo|ok.objects.all()\n    from other.models import Book",
		"from myapp.models import Book\ndef run():\n    for Book in values:\n        Bo|ok.objects.all()",
		"from myapp.models import Book\ndef run():\n    with resource() as Book:\n        Bo|ok.objects.all()",
		"from myapp.models import Book\ndef run():\n    Book, other = pair\n    Bo|ok.objects.all()",
		"from myapp.models import Book\ndef run():\n    for other, Book in values:\n        Bo|ok.objects.all()",
		"from myapp.models import Book\ndef run():\n    Bo|ok.objects.all()\n    def Book():\n        pass",
		"from myapp.models import Book\ndef run():\n    Bo|ok.objects.all()\n    class Book:\n        pass",
		"from myapp.models import Book\ndef run(Book: Callable[[int], int]):\n    Bo|ok.objects.all()",
		"from myapp.models import Book\ndef run():\n    Bo|ok.objects.all()\n    other = Book = dynamic()",
		"text = \"Bo|ok\"",
		"# Bo|ok",
	} {
		source, offset := sourceAtCursor(t, sourceWithCursor)
		store, err := NewStore()
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Open("file:///unknown.py", 1, string(source)); err != nil {
			t.Fatal(err)
		}
		snapshot, _ := store.Snapshot("file:///unknown.py")
		if sourceRange, ok := ResolveDefinitionSyntax(source, offset, graph, snapshot.Syntax); ok {
			t.Fatalf("ResolveDefinitionSyntax(%q) = %#v", sourceWithCursor, sourceRange)
		}
		store.CloseAll()
	}
}
