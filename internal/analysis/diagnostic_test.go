package analysis

import (
	"testing"
)

func TestDiagnoseORMClassifiesStaticPaths(t *testing.T) {
	graph := pathTestGraph(t)
	tests := []struct {
		name   string
		source string
		code   PathIssueCode
		text   string
	}{
		{"unknown root field", "Book.objects.filter(titel=1)", IssueUnknownPathSegment, "titel"},
		{"unknown deep field", "Book.objects.exclude(author__naem='x')", IssueUnknownPathSegment, "naem"},
		{"invalid lookup", "Book.objects.filter(title__bogus=1)", IssueInvalidLookup, "bogus"},
		{"nonterminal unsupported lookup", "Book.objects.filter(title__contains__exact=1)", IssueInvalidLookup, "contains"},
		{"invalid transformed lookup", "Book.objects.filter(published_at__date__hour=1)", IssueInvalidLookup, "hour"},
		{"invalid projection", "Book.objects.values(\"title__icontains\")", IssueInvalidProjection, "icontains"},
		{"nonrelation traversal", "Book.objects.only(\"title__name\")", IssueNonRelationTraversal, "name"},
		{"invalid select target", "Book.objects.select_related(\"store\")", IssueInvalidSelectRelated, "store"},
		{"invalid prefetch name", "Book.objects.prefetch_related(\"store\")", IssueUnknownPathSegment, "store"},
		{"Unicode typo", "Book.objects.filter(author__caféx=1)", IssueUnknownPathSegment, "caféx"},
		{"leading argument comment", "Book.objects.filter(\n    # typo\n    titel=1\n)", IssueUnknownPathSegment, "titel"},
		{"empty projection segment", "Book.objects.values(\"author____name\")", IssueUnknownPathSegment, "__"},
		{"empty projection path", "Book.objects.values(\"\")", IssueUnknownPathSegment, ""},
		{"unrelated field still flagged after annotate", "Book.objects.annotate(count=Count(\"id\")).values_list(\"bogus\")", IssueUnknownPathSegment, "bogus"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := "from myapp.models import Book\n" + test.source + "\n"
			snapshot := diagnosticSnapshot(t, source)
			issues := DiagnoseORM(snapshot, graph)
			if len(issues) != 1 {
				t.Fatalf("DiagnoseORM() = %#v", issues)
			}
			issue := issues[0]
			if issue.Code != test.code || string(snapshot.Source[issue.Range.Start:issue.Range.End]) != test.text {
				t.Fatalf("issue = %#v, highlighted %q", issue, snapshot.Source[issue.Range.Start:issue.Range.End])
			}
		})
	}
}

func TestDiagnoseORMSuppressesValidDynamicAndIncompleteCode(t *testing.T) {
	graph := pathTestGraph(t)
	tests := []string{
		"Book.objects.filter(title='x')",
		"Book.objects.filter(pk=1)",
		"Book.objects.filter(author__pk=1)",
		"Book.objects.filter(author__name__icontains='x')",
		"Book.objects.filter(published_at__date__gte=value)",
		"Book.objects.filter(title__contains='x')",
		"Book.objects.values(\"published_at__date\")",
		"Book.objects.only(\"author__name\")",
		"Book.objects.select_related(\"author__profile\")",
		"Book.objects.prefetch_related(\"store_set\")",
		"Book.objects.filter(**filters)",
		"Book.objects.values(dynamic_path)",
		"Book.objects.values(f\"{field}\")",
		"Book.objects.values(\"author__\" \"missing\")",
		"Book.objects.filter(titel=1",
		"Book.objects.filter(titel=)",
		"[Book.objects.filter(titel=1) for in items]",
		"Book.objects.order_by(\"missing\")",
		"unknown.objects.filter(titel=1)",
		"Book.catalog.filter(titel=1)",
		"Book.objects.values(\"title\").annotate(count=Count(\"id\")).values_list(\"title\", \"count\")",
		"Book.objects.annotate(count=Count(\"id\")).filter(count__gt=1)",
	}
	for _, expression := range tests {
		t.Run(expression, func(t *testing.T) {
			snapshot := diagnosticSnapshot(t, "from myapp.models import Book\n"+expression+"\n")
			if issues := DiagnoseORM(snapshot, graph); len(issues) != 0 {
				t.Fatalf("DiagnoseORM() = %#v", issues)
			}
		})
	}
}

func TestDiagnoseORMReturnsStableSourceOrder(t *testing.T) {
	graph := pathTestGraph(t)
	snapshot := diagnosticSnapshot(t, "from myapp.models import Book\nBook.objects.values(\"bad1\", \"author__bad2\")\n")
	issues := DiagnoseORM(snapshot, graph)
	if len(issues) != 2 || string(snapshot.Source[issues[0].Range.Start:issues[0].Range.End]) != "bad1" || string(snapshot.Source[issues[1].Range.Start:issues[1].Range.End]) != "bad2" {
		t.Fatalf("DiagnoseORM() = %#v", issues)
	}
}

func diagnosticSnapshot(t *testing.T, source string) Snapshot {
	t.Helper()
	store, err := NewStore()
	if err != nil {
		t.Fatal(err)
	}
	defer store.CloseAll()
	if err := store.Open("file:///diagnostic.py", 1, source); err != nil {
		t.Fatal(err)
	}
	snapshot, ok := store.Snapshot("file:///diagnostic.py")
	if !ok {
		t.Fatal("snapshot is missing")
	}
	return snapshot
}
