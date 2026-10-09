package lsp

import (
	"slices"
	"strings"
	"testing"

	"github.com/amirhasanzadehpy/Pogo/internal/analysis"
	protocol "github.com/tliron/glsp/protocol_3_16"
)

const serializerImports = "from rest_framework import serializers\nfrom myapp.models import Book\n"

func TestSerializerCompletion(t *testing.T) {
	tests := []struct {
		name, source string
		want         []string
	}{
		{"model fields", serializerImports + "class BookSerializer(serializers.ModelSerializer):\n    class Meta:\n        model = Book\n        fields = ['tit|']\n", []string{"title"}},
		{"model after fields", serializerImports + "class BookSerializer(serializers.ModelSerializer):\n    class Meta:\n        fields = ('tit|',)\n        model = Book\n", []string{"title"}},
		{"direct alias", "from rest_framework.serializers import ModelSerializer as Base\nfrom myapp.models import Book as Item\nclass S(Base):\n    class Meta:\n        model = Item\n        fields = ['tit|']\n", []string{"title"}},
		{"module alias", "import rest_framework.serializers as api\nfrom myapp.models import Book\nclass S(api.ModelSerializer):\n    class Meta:\n        model = Book\n        fields = ['tit|']\n", []string{"title"}},
		{"hyperlinked", serializerImports + "class S(serializers.HyperlinkedModelSerializer):\n    class Meta:\n        model = Book\n        fields = ['tit|']\n", []string{"title"}},
		{"exclude tuple", serializerImports + "class S(serializers.ModelSerializer):\n    class Meta:\n        model = Book\n        exclude = ('tit|',)\n", []string{"title"}},
		{"explicit methods", serializerImports + "class S(serializers.ModelSerializer):\n    title_display = serializers.SerializerMethodField()\n    class Meta:\n        model = Book\n        fields = ['tit|']\n", []string{"title", "title_display"}},
		{"override deduplication", serializerImports + "class S(serializers.ModelSerializer):\n    title = serializers.CharField()\n    class Meta:\n        model = Book\n        fields = ['tit|']\n", []string{"title"}},
		{"read only selected explicit", serializerImports + "class S(serializers.ModelSerializer):\n    display = serializers.SerializerMethodField()\n    class Meta:\n        model = Book\n        fields = ['title','display']\n        read_only_fields = ['di|']\n", []string{"display"}},
		{"read only all", serializerImports + "class S(serializers.ModelSerializer):\n    class Meta:\n        model = Book\n        fields = '__all__'\n        read_only_fields = ['tit|']\n", []string{"title"}},
		{"read only with exclude", serializerImports + "class S(serializers.ModelSerializer):\n    class Meta:\n        model = Book\n        exclude = ['author']\n        read_only_fields = ['tit|']\n", []string{"title"}},
		{"extra kwargs keys", serializerImports + "class S(serializers.ModelSerializer):\n    class Meta:\n        model = Book\n        fields = ['title']\n        extra_kwargs = {'tit|': {'required': False}}\n", []string{"title"}},
		{"source relation", serializerImports + "class S(serializers.ModelSerializer):\n    label = serializers.CharField(source='author.na|')\n    class Meta:\n        model = Book\n        fields = ['label']\n", []string{"name"}},
		{"source attname", serializerImports + "class S(serializers.ModelSerializer):\n    label = serializers.IntegerField(source='author_i|')\n    class Meta:\n        model = Book\n        fields = ['label']\n", []string{"author_id"}},
		{"source reverse accessor", "from rest_framework import serializers\nfrom myapp.models import Author\nclass S(serializers.ModelSerializer):\n    items = serializers.ReadOnlyField(source='boo|')\n    class Meta:\n        model = Author\n        fields = ['items']\n", []string{"books"}},
		{"multiline comments", serializerImports + "class S(\n    serializers.ModelSerializer,\n):\n    class Meta:\n        model = Book\n        fields = [\n            # A field\n            'tit|',\n        ]\n", []string{"title"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			features := testFeatures(t)
			defer features.Close()
			source, position := lspSourceAtCursor(t, tt.source)
			const uri = "file:///workspace/serializers.py"
			if err := features.documents.Open(uri, 1, string(source)); err != nil {
				t.Fatal(err)
			}
			result, err := features.Completion(uri, position)
			if err != nil || result == nil {
				t.Fatalf("Completion() = %#v, %v", result, err)
			}
			var labels []string
			for _, item := range result.Items {
				labels = append(labels, item.Label)
			}
			if !slices.Equal(labels, tt.want) {
				t.Fatalf("labels = %v, want %v", labels, tt.want)
			}
			edit, ok := result.Items[0].TextEdit.(protocol.TextEdit)
			if !ok || edit.Range.End != (protocol.Position{Line: position.Line, Character: position.Character}) || edit.NewText != tt.want[0] {
				t.Fatalf("edit = %#v", result.Items[0].TextEdit)
			}
		})
	}
}

func TestSerializerConservativeOmissions(t *testing.T) {
	tests := []struct{ name, source string }{
		{"plain serializer", serializerImports + "class S(serializers.Serializer):\n    class Meta:\n        model = Book\n        fields = ['tit|']\n"},
		{"unrelated base", "from other import serializers\nfrom myapp.models import Book\nclass S(serializers.ModelSerializer):\n    class Meta:\n        model = Book\n        fields = ['tit|']\n"},
		{"shadowed module", serializerImports + "serializers = unknown\nclass S(serializers.ModelSerializer):\n    class Meta:\n        model = Book\n        fields = ['tit|']\n"},
		{"custom inheritance", serializerImports + "class Base(serializers.ModelSerializer): pass\nclass S(Base):\n    class Meta:\n        model = Book\n        fields = ['tit|']\n"},
		{"dynamic model", serializerImports + "class S(serializers.ModelSerializer):\n    class Meta:\n        model = choose_model()\n        fields = ['tit|']\n"},
		{"conditional model", serializerImports + "class S(serializers.ModelSerializer):\n    class Meta:\n        if enabled:\n            model = Book\n        fields = ['tit|']\n"},
		{"dynamic fields", serializerImports + "class S(serializers.ModelSerializer):\n    class Meta:\n        model = Book\n        fields = ['tit|'] + extra\n"},
		{"computed source", serializerImports + "class S(serializers.ModelSerializer):\n    label = serializers.CharField(source='author.' + 'na|')\n    class Meta:\n        model = Book\n        fields = ['label']\n"},
		{"source wildcard", serializerImports + "class S(serializers.ModelSerializer):\n    label = serializers.CharField(source='*|')\n    class Meta:\n        model = Book\n        fields = ['label']\n"},
		{"source collection traversal", "from rest_framework import serializers\nfrom myapp.models import Author\nclass S(serializers.ModelSerializer):\n    label = serializers.CharField(source='books.tit|')\n    class Meta:\n        model = Author\n        fields = ['label']\n"},
		{"source attname traversal", serializerImports + "class S(serializers.ModelSerializer):\n    label = serializers.CharField(source='author_id.na|')\n    class Meta:\n        model = Book\n        fields = ['label']\n"},
		{"source scalar traversal", serializerImports + "class S(serializers.ModelSerializer):\n    label = serializers.CharField(source='title.na|')\n    class Meta:\n        model = Book\n        fields = ['label']\n"},
		{"nested kwargs key", serializerImports + "class S(serializers.ModelSerializer):\n    class Meta:\n        model = Book\n        fields = ['title']\n        extra_kwargs = {'title': {'tit|': True}}\n"},
		{"kwargs value", serializerImports + "class S(serializers.ModelSerializer):\n    class Meta:\n        model = Book\n        fields = ['title']\n        extra_kwargs = {'title': {'help_text': 'tit|'}}\n"},
		{"unselected read only", serializerImports + "class S(serializers.ModelSerializer):\n    class Meta:\n        model = Book\n        fields = ['author']\n        read_only_fields = ['tit|']\n"},
		{"excluded read only", serializerImports + "class S(serializers.ModelSerializer):\n    class Meta:\n        model = Book\n        exclude = ['title']\n        read_only_fields = ['tit|']\n"},
		{"explicit kwargs ignored", serializerImports + "class S(serializers.ModelSerializer):\n    title = serializers.CharField()\n    class Meta:\n        model = Book\n        fields = ['title']\n        extra_kwargs = {'tit|': {'required': False}}\n"},
		{"unknown override", serializerImports + "class S(serializers.ModelSerializer):\n    title = custom_field()\n    class Meta:\n        model = Book\n        fields = ['tit|']\n"},
		{"fstring", serializerImports + "class S(serializers.ModelSerializer):\n    class Meta:\n        model = Book\n        fields = [f'tit|']\n"},
		{"escaped literal", serializerImports + "class S(serializers.ModelSerializer):\n    class Meta:\n        model = Book\n        fields = ['tit\\x6c|']\n"},
		{"property source", serializerImports + "class S(serializers.ModelSerializer):\n    label = serializers.CharField(source='computed.na|')\n    class Meta:\n        model = Book\n        fields = ['label']\n"},
		{"shadowed model in Meta", serializerImports + "class S(serializers.ModelSerializer):\n    class Meta:\n        Book = dynamic_model()\n        model = Book\n        fields = ['tit|']\n"},
		{"shadowed model in serializer", serializerImports + "class S(serializers.ModelSerializer):\n    Book = dynamic_model()\n    class Meta:\n        model = Book\n        fields = ['tit|']\n"},
		{"annotated unknown override", serializerImports + "class S(serializers.ModelSerializer):\n    title: str = custom_field()\n    class Meta:\n        model = Book\n        fields = ['tit|']\n"},
		{"unrecognized DRF field", serializerImports + "class S(serializers.ModelSerializer):\n    title = serializers.DoesNotExistField()\n    class Meta:\n        model = Book\n        fields = ['tit|']\n"},
		{"combined constructor", serializerImports + "class S(serializers.ModelSerializer):\n    label = serializers.CharField(source='author.na|') + transform()\n    class Meta:\n        model = Book\n        fields = ['label']\n"},
		{"incomplete literal", serializerImports + "class S(serializers.ModelSerializer):\n    class Meta:\n        model = Book\n        fields = ['tit|\n"},
		{"conditional serializer", serializerImports + "if enabled:\n    class S(serializers.ModelSerializer):\n        class Meta:\n            model = Book\n            fields = ['tit|']\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			features := testFeatures(t)
			defer features.Close()
			source, position := lspSourceAtCursor(t, tt.source)
			const uri = "file:///workspace/serializers.py"
			if err := features.documents.Open(uri, 1, string(source)); err != nil {
				t.Fatal(err)
			}
			result, err := features.Completion(uri, position)
			if err != nil || result != nil {
				t.Fatalf("Completion() = %#v, %v", result, err)
			}
			hover, err := features.Hover(uri, position)
			if err != nil || hover != nil {
				t.Fatalf("Hover() = %#v, %v", hover, err)
			}
		})
	}
}

func TestSerializerHoverAndLocalDefinition(t *testing.T) {
	features := testFeatures(t)
	defer features.Close()
	const uri = "file:///workspace/serializers.py"
	source, position := lspSourceAtCursor(t, serializerImports+"class S(serializers.ModelSerializer):\n    display = serializers.CharField(source='author.name')\n    class Meta:\n        model = Book\n        fields = ['😀', 'dis|play']\n")
	if err := features.documents.Open(uri, 1, string(source)); err != nil {
		t.Fatal(err)
	}
	hover, err := features.Hover(uri, position)
	if err != nil || hover == nil {
		t.Fatalf("Hover() = %#v, %v", hover, err)
	}
	markup := hover.Contents.(protocol.MarkupContent)
	for _, want := range []string{"DRF serializer field", "Source attribute: `author.name`", "CharField"} {
		if !strings.Contains(markup.Value, want) {
			t.Fatalf("hover missing %q: %s", want, markup.Value)
		}
	}
	if hover.Range.Start.Character != 25 || hover.Range.End.Character != 32 {
		t.Fatalf("UTF-16 range = %#v", hover.Range)
	}
	location, err := features.Definition(uri, position)
	if err != nil || location == nil || string(location.URI) != uri || location.Range.Start != (protocol.Position{Line: 3, Character: 4}) || location.Range.End.Character != 11 {
		t.Fatalf("Definition() = %#v, %v", location, err)
	}
	snapshot, _ := features.documents.Snapshot(uri)
	graph, _ := features.cache.Load()
	if issues := analysis.DiagnoseORM(snapshot, graph); len(issues) != 0 {
		t.Fatalf("serializer properties must not receive ORM diagnostics: %#v", issues)
	}
}

func TestSerializerModelDefinition(t *testing.T) {
	features, modelsPath := navigationTestFeatures(t)
	defer features.Close()
	for _, reference := range []string{"fields = ['tit|le']", "fields = ['title']"} {
		sourceText := serializerImports + "class S(serializers.ModelSerializer):\n"
		if !strings.Contains(reference, "|") {
			sourceText += "    display = serializers.CharField(source='author.na|me')\n"
		}
		sourceText += "    class Meta:\n        model = Book\n        " + reference + "\n"
		source, position := lspSourceAtCursor(t, sourceText)
		const uri = "file:///workspace/serializers.py"
		if err := features.documents.Open(uri, 1, string(source)); err != nil {
			t.Fatal(err)
		}
		location, err := features.Definition(uri, position)
		if err != nil || location == nil {
			t.Fatalf("Definition() = %#v, %v", location, err)
		}
		want, _ := sourceFileURI(modelsPath)
		if location.URI != want {
			t.Fatalf("definition URI = %s, want %s", location.URI, want)
		}
		wantLine := protocol.UInteger(5)
		if !strings.Contains(reference, "|") {
			wantLine = 2
		}
		if location.Range.Start.Line != wantLine {
			t.Fatalf("definition line = %d, want %d", location.Range.Start.Line, wantLine)
		}
	}
}

func TestSerializerLocalDefinitionFollowsUnsavedEdit(t *testing.T) {
	features := testFeatures(t)
	defer features.Close()
	const uri = "file:///workspace/serializers.py"
	source, _ := lspSourceAtCursor(t, serializerImports+"class S(serializers.ModelSerializer):\n    display = serializers.SerializerMethodField()\n    class Meta:\n        model = Book\n        fields = ['dis|play']\n")
	if err := features.documents.Open(uri, 1, string(source)); err != nil {
		t.Fatal(err)
	}
	updated, position := lspSourceAtCursor(t, serializerImports+"class S(serializers.ModelSerializer):\n    # unsaved new line\n    display = serializers.SerializerMethodField()\n    class Meta:\n        model = Book\n        fields = ['dis|play']\n")
	if err := features.documents.Change(uri, 2, []analysis.Change{{Text: string(updated)}}); err != nil {
		t.Fatal(err)
	}
	location, err := features.Definition(uri, position)
	if err != nil || location == nil || location.Range.Start.Line != 4 {
		t.Fatalf("Definition after edit = %#v, %v", location, err)
	}
}

func BenchmarkSerializerCompletion(b *testing.B) {
	features := testFeatures(b)
	defer features.Close()
	source, position := lspSourceAtCursor(b, serializerImports+"class S(serializers.ModelSerializer):\n    display = serializers.CharField(source='author.name')\n    class Meta:\n        model = Book\n        fields = ['tit|']\n")
	const uri = "file:///workspace/serializers.py"
	if err := features.documents.Open(uri, 1, string(source)); err != nil {
		b.Fatal(err)
	}
	benchmarkNavigationLatency(b, func() {
		result, err := features.Completion(uri, position)
		if err != nil || result == nil {
			b.Fatalf("Completion() = %#v, %v", result, err)
		}
	})
}
