package analysis

import (
	"strings"
	"testing"
)

func FuzzSerializerContext(f *testing.F) {
	f.Add("['title']", uint32(3))
	f.Add("{'title': {'source': 'author.name'}}", uint32(3))
	f.Add("['\\\\', f'title', '\"']", uint32(4))
	f.Add(strings.Repeat("['title'],", 100), uint32(20))
	graph := pathTestGraph(f)
	store, err := NewStore()
	if err != nil {
		f.Fatal(err)
	}
	defer store.CloseAll()
	const prefix = "from rest_framework import serializers\nfrom myapp.models import Book\nclass S(serializers.ModelSerializer):\n    class Meta:\n        model = Book\n        fields = "
	f.Fuzz(func(t *testing.T, expression string, position uint32) {
		if len(expression) > maxSerializerExpressionBytes+1 {
			t.Skip()
		}
		source := prefix + expression + "\n"
		if err := store.Open("file:///serializers.py", 1, source); err != nil {
			return
		}
		snapshot, _ := store.Snapshot("file:///serializers.py")
		offset := len(prefix) + int(position%uint32(len(expression)+1))
		context, ok := analyzeSerializerContext(snapshot.Source, offset, graph, snapshot.Syntax)
		if ok {
			if context.Replacement.Start < len(prefix) || context.Replacement.End > len(source) || context.Replacement.Start > context.Replacement.End {
				t.Fatalf("invalid replacement: %#v", context.Replacement)
			}
			for _, field := range context.SerializerFields {
				if field.Name == "" {
					t.Fatal("empty candidate")
				}
			}
		}
	})
}

func TestSerializerTokenBounds(t *testing.T) {
	for _, source := range []string{strings.Repeat("x", maxSerializerExpressionBytes+1), strings.Repeat("x ", maxSerializerTokens+1)} {
		if _, ok := serializerTokens([]byte(source), ByteRange{0, len(source)}); ok {
			t.Fatal("oversized expression accepted")
		}
	}
}
