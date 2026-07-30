package analysis

import (
	"strings"
	"testing"
)

func TestStoreOpenIncrementalFullAndClose(t *testing.T) {
	store := newTestStore(t)
	uri := "file:///workspace/example.py"
	initial := "from myapp.models import Book\nBook.objects.filter(au)\n"
	if err := store.Open(uri, 1, initial); err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	opened, ok := store.Snapshot(uri)
	if !ok || len(opened.Syntax) != 1 || !strings.Contains(opened.Syntax[0].Text, "import Book") {
		t.Fatalf("CST symbol query = %#v", opened.Syntax)
	}
	if err := store.Change(uri, 2, []Change{{
		Range: &Range{Start: Position{Line: 1, Character: 20}, End: Position{Line: 1, Character: 22}},
		Text:  "author",
	}}); err != nil {
		t.Fatalf("incremental Change() error = %v", err)
	}
	snapshot, ok := store.Snapshot(uri)
	if !ok || !snapshot.Parsed || snapshot.Version != 2 || !strings.Contains(string(snapshot.Source), "filter(author)") {
		t.Fatalf("incremental snapshot = %#v, %q", snapshot, snapshot.Source)
	}
	if err := store.Change(uri, 3, []Change{{Text: "Book.objects.filter(title=\n"}}); err != nil {
		t.Fatalf("full Change() error = %v", err)
	}
	snapshot, ok = store.Snapshot(uri)
	if !ok || !snapshot.Parsed || snapshot.Version != 3 || string(snapshot.Source) != "Book.objects.filter(title=\n" {
		t.Fatalf("full snapshot = %#v, %q", snapshot, snapshot.Source)
	}
	store.Close(uri)
	if _, ok := store.Snapshot(uri); ok {
		t.Fatal("closed document remains in store")
	}
}

func TestUTF16PositionConversionRejectsSurrogateInterior(t *testing.T) {
	source := []byte("prefix = '😀'\nBook.objects.filter(au)\n")
	offset, ok := ByteOffset(source, Position{Line: 0, Character: 10})
	if !ok || string(source[offset:]) != "😀'\nBook.objects.filter(au)\n" {
		t.Fatalf("emoji start offset = %d, %v", offset, ok)
	}
	if _, ok := ByteOffset(source, Position{Line: 0, Character: 11}); ok {
		t.Fatal("position inside surrogate pair was accepted")
	}
	afterEmoji, ok := ByteOffset(source, Position{Line: 0, Character: 12})
	if !ok {
		t.Fatal("position after emoji was rejected")
	}
	position, ok := PositionAt(source, afterEmoji)
	if !ok || position != (Position{Line: 0, Character: 12}) {
		t.Fatalf("round-trip position = %#v, %v", position, ok)
	}
}

func TestInvalidChangePreservesDocument(t *testing.T) {
	store := newTestStore(t)
	uri := "file:///workspace/example.py"
	if err := store.Open(uri, 4, "value = '😀'\n"); err != nil {
		t.Fatal(err)
	}
	invalid := []Change{{
		Range: &Range{Start: Position{Line: 0, Character: 10}, End: Position{Line: 0, Character: 11}},
		Text:  "x",
	}}
	if err := store.Change(uri, 5, invalid); err == nil {
		t.Fatal("surrogate-interior Change() error = nil")
	}
	batch := []Change{
		{Range: &Range{Start: Position{Line: 0, Character: 0}, End: Position{Line: 0, Character: 5}}, Text: "item"},
		{Range: &Range{Start: Position{Line: 9}, End: Position{Line: 9}}, Text: "invalid"},
	}
	if err := store.Change(uri, 5, batch); err == nil {
		t.Fatal("partially valid Change() error = nil")
	}
	if err := store.Change(uri, 4, []Change{{Text: "stale"}}); err == nil {
		t.Fatal("stale Change() error = nil")
	}
	snapshot, _ := store.Snapshot(uri)
	if snapshot.Version != 4 || string(snapshot.Source) != "value = '😀'\n" {
		t.Fatalf("invalid change modified snapshot = %#v, %q", snapshot, snapshot.Source)
	}
	if err := store.Change(uri, 5, []Change{{
		Range: &Range{Start: Position{Line: 0, Character: 0}, End: Position{Line: 0, Character: 5}},
		Text:  "item",
	}}); err != nil {
		t.Fatalf("valid change after rejected batch = %v", err)
	}
}

func BenchmarkStoreIncrementalChange(b *testing.B) {
	for range b.N {
		store, err := NewStore()
		if err != nil {
			b.Fatal(err)
		}
		if err := store.Open("file:///bench.py", 1, "from myapp.models import Book\nBook.objects.filter(au)\n"); err != nil {
			b.Fatal(err)
		}
		if err := store.Change("file:///bench.py", 2, []Change{{
			Range: &Range{Start: Position{Line: 1, Character: 20}, End: Position{Line: 1, Character: 22}},
			Text:  "author",
		}}); err != nil {
			b.Fatal(err)
		}
		store.Close("file:///bench.py")
	}
}

func FuzzStoreParserRecovery(f *testing.F) {
	for _, seed := range []string{
		"Book.objects.filter(",
		"from myapp.models import (\n    Book,\n",
		"def incomplete(value:\n    return value.",
		"text = '😀'\nBook.objects.filter(author=func(value),",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, text string) {
		if len(text) > 64*1024 {
			t.Skip()
		}
		store := newTestStore(t)
		uri := "file:///fuzz.py"
		if err := store.Open(uri, 1, text); err != nil {
			return
		}
		if err := store.Change(uri, 2, []Change{{Text: text + "\n# changed"}}); err != nil {
			t.Fatalf("full recovery change: %v", err)
		}
		store.Close(uri)
	})
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := NewStore()
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	return store
}
