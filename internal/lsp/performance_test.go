package lsp

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/amirhasanzadehpy/Pogo/internal/analysis"
	"github.com/amirhasanzadehpy/Pogo/internal/schema"
	protocol "github.com/tliron/glsp/protocol_3_16"
)

func BenchmarkCompletionScale(b *testing.B) {
	b.Run("self-cycle-depth-32", func(b *testing.B) {
		features := scaleTestFeatures(b, 1, 0)
		defer features.Close()
		path := strings.Repeat("parent__", 32) + "na"
		source, position := lspSourceAtCursor(b, "from myapp.models import Node\nNode.objects.filter("+path+"|)")
		uri := "file:///self-cycle.py"
		if err := features.documents.Open(uri, 1, string(source)); err != nil {
			b.Fatal(err)
		}
		benchmarkNavigationLatency(b, func() {
			result, err := features.Completion(uri, position)
			if err != nil || result == nil || len(result.Items) != 1 || result.Items[0].Label != "name" {
				b.Fatalf("Completion() = %#v, %v", result, err)
			}
		})
	})

	b.Run("dense-relations-256", func(b *testing.B) {
		features := scaleTestFeatures(b, 1, 256)
		defer features.Close()
		source, position := lspSourceAtCursor(b, "from myapp.models import Node\nNode.objects.filter(relation_|)")
		uri := "file:///dense-relations.py"
		if err := features.documents.Open(uri, 1, string(source)); err != nil {
			b.Fatal(err)
		}
		benchmarkNavigationLatency(b, func() {
			result, err := features.Completion(uri, position)
			if err != nil || result == nil || len(result.Items) != 256 {
				b.Fatalf("Completion() items = %d, %v", completionLength(result), err)
			}
		})
	})

	b.Run("models-10000", func(b *testing.B) {
		features := scaleTestFeatures(b, 10_000, 0)
		defer features.Close()
		source, position := lspSourceAtCursor(b, "from myapp.models import Model9999\nModel9999.objects.filter(rela|)")
		uri := "file:///models-10000.py"
		if err := features.documents.Open(uri, 1, string(source)); err != nil {
			b.Fatal(err)
		}
		benchmarkNavigationLatency(b, func() {
			result, err := features.Completion(uri, position)
			if err != nil || result == nil || len(result.Items) != 1 || result.Items[0].Label != "relation" {
				b.Fatalf("Completion() = %#v, %v", result, err)
			}
		})
	})
}

func BenchmarkHoverScale(b *testing.B) {
	features := scaleTestFeatures(b, 1, 0)
	defer features.Close()
	path := strings.Repeat("parent__", 32) + "name"
	source := "from myapp.models import Node\nNode.objects.filter(" + path + "=1)\n"
	uri := "file:///hover-self-cycle.py"
	if err := features.documents.Open(uri, 1, source); err != nil {
		b.Fatal(err)
	}
	offset := strings.LastIndex(source, "name") + 2
	position, _ := analysis.PositionAt([]byte(source), offset)
	benchmarkNavigationLatency(b, func() {
		hover, err := features.Hover(uri, position)
		if err != nil || hover == nil {
			b.Fatalf("Hover() = %#v, %v", hover, err)
		}
	})
}

func BenchmarkDiagnosticScale(b *testing.B) {
	features := scaleTestFeatures(b, 1, 0)
	defer features.Close()
	features.SetNotifier(func(string, any) {})
	var source strings.Builder
	source.WriteString("from myapp.models import Node\n")
	for range 100 {
		source.WriteString("Node.objects.filter(parent__missing=1)\n")
	}
	uri := "file:///diagnostic-scale.py"
	if err := features.documents.Open(uri, 1, source.String()); err != nil {
		b.Fatal(err)
	}
	benchmarkNavigationLatency(b, func() { features.publishURI(uri) })
}

func completionLength(result *protocol.CompletionList) int {
	if result == nil {
		return 0
	}
	return len(result.Items)
}

func scaleTestFeatures(b *testing.B, modelCount, denseRelations int) *Features {
	b.Helper()
	forward := "forward"
	manyToOne := "many-to-one"
	models := make(map[string]schema.Model, modelCount+1)
	if modelCount == 1 {
		related := "myapp.Node"
		fields := map[string]schema.Field{
			"name": featureTestField("myapp.Node", "name", "django.db.models.CharField"),
			"parent": {
				Type: "django.db.models.ForeignKey", InternalType: "ForeignKey", Name: "parent", IsRelation: true,
				RelatedModel: &related, SourceModel: "myapp.Node", SourceRange: featureTestSourceRange(), RelationDirection: &forward, RelationCardinality: &manyToOne,
			},
		}
		for index := 0; index < denseRelations; index++ {
			name := fmt.Sprintf("relation_%03d", index)
			fields[name] = schema.Field{
				Type: "django.db.models.ForeignKey", InternalType: "ForeignKey", Name: name, IsRelation: true,
				RelatedModel: &related, SourceModel: "myapp.Node", SourceRange: featureTestSourceRange(), RelationDirection: &forward, RelationCardinality: &manyToOne,
			}
		}
		models["Node"] = featureTestModel("Node", fields)
	} else {
		for index := 0; index < modelCount; index++ {
			name := fmt.Sprintf("Model%d", index)
			label := "myapp." + name
			related := fmt.Sprintf("myapp.Model%d", (index+1)%modelCount)
			models[name] = featureTestModel(name, map[string]schema.Field{
				"relation": {
					Type: "django.db.models.ForeignKey", InternalType: "ForeignKey", Name: "relation", IsRelation: true,
					RelatedModel: &related, SourceModel: label, SourceRange: featureTestSourceRange(), RelationDirection: &forward, RelationCardinality: &manyToOne,
				},
			})
		}
	}
	graph, err := schema.Build(schema.Snapshot{
		SchemaVersion: 1, PositionEncoding: "utf-8-bytes", LookupTransformMaxDepth: 2, LookupPathMaxCount: 512,
		Apps: map[string]schema.App{"myapp": {Label: "myapp", ImportName: "myapp", RootPath: filepath.Dir(featureTestModelPath()), Models: models}},
	})
	if err != nil {
		b.Fatal(err)
	}
	cache := &schema.Cache{}
	cache.Replace(graph)
	features, err := NewFeatures(cache)
	if err != nil {
		b.Fatal(err)
	}
	return features
}
