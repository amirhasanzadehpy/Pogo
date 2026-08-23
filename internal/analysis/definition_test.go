package analysis

import (
	"strings"
	"testing"
)

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

func TestResolveAnnotationAliasDefinition(t *testing.T) {
	graph := inferenceTestGraph(t)
	tests := []struct {
		name   string
		source string
	}{
		{"values_list argument", "from myapp.models import Author\nAuthor.objects.annotate(count=Count('id')).values_list(\"pk\", \"cou|nt\")\n"},
		{"filter lookup", "from myapp.models import Author\nAuthor.objects.annotate(count=Count('id')).filter(cou|nt__gt=1)\n"},
		{"through first()", "from myapp.models import Author\nAuthor.objects.annotate(has_books=Exists(p)).first().has_boo|ks\n"},
		{"second annotate call", "from myapp.models import Author\nAuthor.objects.annotate(a=Value(1)).annotate(b=Value(2)).values_list(\"|b\")\n"},
		{"F() argument referencing an earlier annotate", "from django.db.models import F\nfrom myapp.models import Author\nAuthor.objects.annotate(count=Count('id')).annotate(total=F(\"cou|nt\") + 1)\n"},
		{"Sum() argument referencing an earlier annotate", "from django.db.models import Sum\nfrom myapp.models import Author\nAuthor.objects.annotate(a=Value(1)).annotate(b=Sum(\"|a\"))\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source, offset := sourceAtCursor(t, test.source)
			store, err := NewStore()
			if err != nil {
				t.Fatal(err)
			}
			defer store.CloseAll()
			if err := store.Open("file:///annotate-def.py", 1, string(source)); err != nil {
				t.Fatal(err)
			}
			snapshot, _ := store.Snapshot("file:///annotate-def.py")
			sourceRange, ok := ResolveAnnotationAliasDefinition(source, offset, graph, snapshot.Syntax, "")
			if !ok {
				t.Fatalf("ResolveAnnotationAliasDefinition() ok = false")
			}
			after := string(source[sourceRange.End:min(sourceRange.End+1, len(source))])
			if after != "=" {
				t.Fatalf("resolved range %q does not point at a keyword name (followed by %q)", source[sourceRange.Start:sourceRange.End], after)
			}
		})
	}
}

func TestAnnotationReturnType(t *testing.T) {
	graph := inferenceTestGraph(t)
	tests := []struct {
		name   string
		source string
		want   string
	}{
		{"count", "from myapp.models import Book\nBook.objects.annotate(count=Count(\"id\")).values_list(\"cou|nt\")\n", "int"},
		{"exists", "from myapp.models import Book\nBook.objects.annotate(has_author=Exists(x)).filter(has_autho|r=True)\n", "bool"},
		{"sum resolves field type", "from myapp.models import Book\nBook.objects.annotate(total=Sum(\"id\")).values_list(\"tota|l\")\n", "django.db.models.fields.BigAutoField"},
		{"f resolves field type", "from myapp.models import Book\nBook.objects.annotate(t=F(\"title\")).values_list(\"|t\")\n", "django.db.models.fields.CharField"},
		{"value int literal", "from myapp.models import Book\nBook.objects.annotate(one=Value(1)).values_list(\"on|e\")\n", "int"},
		{"value string literal", "from myapp.models import Book\nBook.objects.annotate(label=Value(\"x\")).values_list(\"labe|l\")\n", "str"},
		{"explicit output_field wins", "from myapp.models import Book\nBook.objects.annotate(total=Sum(\"id\", output_field=FloatField())).values_list(\"tota|l\")\n", "FloatField"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source, offset := sourceAtCursor(t, test.source)
			store, err := NewStore()
			if err != nil {
				t.Fatal(err)
			}
			defer store.CloseAll()
			if err := store.Open("file:///annotate-type.py", 1, string(source)); err != nil {
				t.Fatal(err)
			}
			snapshot, _ := store.Snapshot("file:///annotate-type.py")
			got, ok := AnnotationReturnType(source, offset, graph, snapshot.Syntax, "")
			if !ok || got != test.want {
				t.Fatalf("AnnotationReturnType() = %q, %v; want %q", got, ok, test.want)
			}
		})
	}
}

func TestResolveAnnotationAliasDefinitionAcrossMultiKeywordAnnotateChain(t *testing.T) {
	graph := inferenceTestGraph(t)
	source := "from django.db.models import Count, F\n" +
		"from myapp.models import Author\n" +
		"(\n" +
		"    Author.objects.prefetch_related(\"book\")\n" +
		"    .annotate(\n" +
		"        email_invitation_count=Count(\"a\", distinct=True),\n" +
		"        invitation_count=Count(\"b\", distinct=True),\n" +
		"        user_count=Count(\"c\", distinct=True) + 1,\n" +
		"    )\n" +
		"    .annotate(total=F(\"user_cou|nt\") + F(\"invitation_count\") + F(\"email_invitation_count\"))\n" +
		"    .distinct()\n" +
		")\n"
	src, offset := sourceAtCursor(t, source)
	store, err := NewStore()
	if err != nil {
		t.Fatal(err)
	}
	defer store.CloseAll()
	if err := store.Open("file:///annotate-chain.py", 1, string(src)); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := store.Snapshot("file:///annotate-chain.py")
	sourceRange, ok := ResolveAnnotationAliasDefinition(src, offset, graph, snapshot.Syntax, "")
	if !ok {
		t.Fatalf("ResolveAnnotationAliasDefinition() ok = false")
	}
	text := string(src[sourceRange.Start:sourceRange.End])
	after := string(src[sourceRange.End:min(sourceRange.End+1, len(src))])
	if text != "user_count" || after != "=" {
		t.Fatalf("resolved range = %q, followed by %q; want \"user_count\" followed by \"=\"", text, after)
	}
	returnType, ok := AnnotationReturnType(src, offset, graph, snapshot.Syntax, "")
	if ok {
		t.Fatalf("AnnotationReturnType() = %q, want unresolved (combined expression)", returnType)
	}

	invitationOffset := strings.Index(string(src), "F(\"invitation_count\")") + len("F(\"")
	if returnType, ok := AnnotationReturnType(src, invitationOffset, graph, snapshot.Syntax, ""); !ok || returnType != "int" {
		t.Fatalf("AnnotationReturnType(invitation_count) = %q, %v; want \"int\"", returnType, ok)
	}
}

func TestAnnotationReturnTypeUnresolved(t *testing.T) {
	graph := inferenceTestGraph(t)
	for _, sourceWithCursor := range []string{
		"from myapp.models import Book\nBook.objects.annotate(t=F(\"bogus\")).values_list(\"|t\")",
		"from myapp.models import Book\nBook.objects.annotate(t=F(\"id\") + F(\"id\")).values_list(\"|t\")",
	} {
		source, offset := sourceAtCursor(t, sourceWithCursor)
		store, err := NewStore()
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Open("file:///annotate-type-reject.py", 1, string(source)); err != nil {
			t.Fatal(err)
		}
		snapshot, _ := store.Snapshot("file:///annotate-type-reject.py")
		if got, ok := AnnotationReturnType(source, offset, graph, snapshot.Syntax, ""); ok {
			t.Fatalf("AnnotationReturnType(%q) = %q, want unresolved", sourceWithCursor, got)
		}
		store.CloseAll()
	}
}

func TestResolveAnnotationAliasDefinitionRejectsNonAnnotations(t *testing.T) {
	graph := inferenceTestGraph(t)
	for _, sourceWithCursor := range []string{
		"from myapp.models import Author\nAuthor.objects.filter(na|me='x')",
		"from myapp.models import Author\nAuthor.objects.annotate(count=Count('id')).values_list(\"bog|us\")",
	} {
		source, offset := sourceAtCursor(t, sourceWithCursor)
		store, err := NewStore()
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Open("file:///annotate-def-reject.py", 1, string(source)); err != nil {
			t.Fatal(err)
		}
		snapshot, _ := store.Snapshot("file:///annotate-def-reject.py")
		if sourceRange, ok := ResolveAnnotationAliasDefinition(source, offset, graph, snapshot.Syntax, ""); ok {
			t.Fatalf("ResolveAnnotationAliasDefinition(%q) = %#v", sourceWithCursor, sourceRange)
		}
		store.CloseAll()
	}
}
