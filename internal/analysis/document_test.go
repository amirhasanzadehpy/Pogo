package analysis

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
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

func FuzzUTF16PositionRoundTrip(f *testing.F) {
	for _, seed := range []string{"ascii\ntext", "café\r\n😀 value", "日本語\nBook.objects.filter()"} {
		f.Add(seed, uint16(0))
	}
	f.Fuzz(func(t *testing.T, text string, rawOffset uint16) {
		source := []byte(text)
		if len(source) == 0 || len(source) > 64*1024 || !utf8.Valid(source) {
			t.Skip()
		}
		offset := int(rawOffset) % (len(source) + 1)
		if offset < len(source) && !utf8.RuneStart(source[offset]) {
			return
		}
		position, ok := PositionAt(source, offset)
		if !ok {
			t.Fatalf("PositionAt() rejected UTF-8 boundary %d", offset)
		}
		roundTrip, ok := ByteOffset(source, position)
		if !ok {
			if offset > 0 && offset < len(source) && source[offset-1] == '\r' && source[offset] == '\n' {
				return
			}
			t.Fatalf("ByteOffset() rejected %#v from boundary %d", position, offset)
		}
		if roundTrip != offset {
			t.Fatalf("offset %d -> %#v -> %d", offset, position, roundTrip)
		}
	})
}

func FuzzUTF16EditMatchesFullParse(f *testing.F) {
	f.Add("value = '😀'\nBook.objects.filter(author__name=1)\n", uint16(8), uint16(12), "café")
	f.Add("from app.models import Book\r\nBook.objects.values(\"title\")\r\n", uint16(0), uint16(4), "import")
	f.Fuzz(func(t *testing.T, text string, rawStart, rawEnd uint16, replacement string) {
		source := []byte(text)
		if len(source) == 0 || len(source) > 16*1024 || len(replacement) > 4096 || !utf8.Valid(source) || !utf8.ValidString(replacement) {
			t.Skip()
		}
		boundaries := []int{0}
		for offset := range source {
			if offset > 0 && utf8.RuneStart(source[offset]) {
				boundaries = append(boundaries, offset)
			}
		}
		boundaries = append(boundaries, len(source))
		start := boundaries[int(rawStart)%len(boundaries)]
		end := boundaries[int(rawEnd)%len(boundaries)]
		if start > end {
			start, end = end, start
		}
		startPosition, startOK := PositionAt(source, start)
		endPosition, endOK := PositionAt(source, end)
		if !startOK || !endOK {
			t.Fatalf("PositionAt() rejected edit boundaries %d:%d", start, end)
		}
		if offset, ok := ByteOffset(source, startPosition); !ok || offset != start {
			if !(start > 0 && start < len(source) && source[start-1] == '\r' && source[start] == '\n') {
				t.Fatalf("start position %#v round trip = %d, %v", startPosition, offset, ok)
			}
			return
		}
		if offset, ok := ByteOffset(source, endPosition); !ok || offset != end {
			if !(end > 0 && end < len(source) && source[end-1] == '\r' && source[end] == '\n') {
				t.Fatalf("end position %#v round trip = %d, %v", endPosition, offset, ok)
			}
			return
		}
		final := append(append(append([]byte(nil), source[:start]...), replacement...), source[end:]...)
		if len(final) > MaxDocumentSize {
			return
		}

		incremental := newTestStore(t)
		defer incremental.CloseAll()
		fresh := newTestStore(t)
		defer fresh.CloseAll()
		if err := incremental.Open("file:///incremental.py", 1, text); err != nil {
			return
		}
		if err := incremental.Change("file:///incremental.py", 2, []Change{{Range: &Range{Start: startPosition, End: endPosition}, Text: replacement}}); err != nil {
			t.Fatalf("incremental change: %v", err)
		}
		if err := fresh.Open("file:///fresh.py", 1, string(final)); err != nil {
			t.Fatalf("fresh open: %v", err)
		}
		got, _ := incremental.Snapshot("file:///incremental.py")
		want, _ := fresh.Snapshot("file:///fresh.py")
		if !bytes.Equal(got.Source, want.Source) || got.Parsed != want.Parsed || !reflect.DeepEqual(got.Syntax, want.Syntax) || !reflect.DeepEqual(got.Calls, want.Calls) {
			t.Fatalf("incremental snapshot differs from full parse\ngot syntax=%#v calls=%#v\nwant syntax=%#v calls=%#v", got.Syntax, got.Calls, want.Syntax, want.Calls)
		}
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
