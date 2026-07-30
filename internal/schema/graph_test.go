package schema

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"
)

func TestBuildIndexesAndRejectsDanglingRelations(t *testing.T) {
	snapshot := syntheticSnapshot(2)
	graph, err := Build(snapshot)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if graph.ModelCount() != 2 || !graph.HasModel("app.Model1") || !graph.HasClass("app.models.Model1") {
		t.Fatalf("model indexes are incomplete")
	}
	if graph.ModuleModelCount("app.models") != 2 {
		t.Fatalf("module model count = %d", graph.ModuleModelCount("app.models"))
	}
	if _, ok := graph.Field("app.Model1", "relation"); !ok {
		t.Fatal("field index misses relation")
	}
	if _, ok := graph.QueryField("app.Model1", "relation"); !ok {
		t.Fatal("query index misses relation")
	}
	if _, ok := graph.AccessorField("app.Model1", "reverse_set"); !ok {
		t.Fatal("accessor index misses relation")
	}
	if _, ok := graph.Manager("app.Model1", "objects"); !ok {
		t.Fatal("manager index misses objects")
	}

	missing := "app.Missing"
	model := snapshot.Apps["app"].Models["Model1"]
	field := model.Fields["relation"]
	field.RelatedModel = &missing
	model.Fields["relation"] = field
	snapshot.Apps["app"].Models["Model1"] = model
	if _, err := Build(snapshot); err == nil {
		t.Fatal("Build() dangling relation error = nil")
	}
}

func TestCacheConcurrentReadersObserveCompleteGenerations(t *testing.T) {
	cache := &Cache{}
	const replacements = 100
	var readers sync.WaitGroup
	stop := make(chan struct{})
	for range 8 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
					graph, generation := cache.Load()
					if generation == 0 {
						if graph != nil {
							t.Error("generation zero has a graph")
						}
						continue
					}
					if graph == nil || graph.ModelCount() != int(generation) {
						t.Errorf("generation %d has incomplete graph", generation)
						return
					}
				}
			}
		}()
	}
	for size := 1; size <= replacements; size++ {
		graph, err := Build(syntheticSnapshot(size))
		if err != nil {
			t.Fatalf("Build(%d) error = %v", size, err)
		}
		if generation := cache.Replace(graph); generation != uint64(size) {
			t.Fatalf("Replace() generation = %d, want %d", generation, size)
		}
	}
	close(stop)
	readers.Wait()
}

func TestGraphFreezesSnapshotAndCacheRejectsNilReplacement(t *testing.T) {
	snapshot := syntheticSnapshot(2)
	graph, err := Build(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	model := snapshot.Apps["app"].Models["Model0"]
	field := model.Fields["relation"]
	missing := "app.Missing"
	field.RelatedModel = &missing
	field.Lookups = append(field.Lookups, "mutated")
	model.Fields["relation"] = field
	snapshot.Apps["app"].Models["Model0"] = model

	frozen, ok := graph.Field("app.Model0", "relation")
	if !ok {
		t.Fatal("frozen field is missing")
	}
	if related, ok := frozen.RelatedModel(); !ok || related != "app.Model1" {
		t.Fatalf("frozen relation = %q, %v", related, ok)
	}
	cache := &Cache{}
	if generation := cache.Replace(graph); generation != 1 {
		t.Fatalf("first generation = %d", generation)
	}
	if generation := cache.Replace(nil); generation != 1 {
		t.Fatalf("nil replacement generation = %d, want 1", generation)
	}
	retained, generation := cache.Load()
	if retained != graph || generation != 1 {
		t.Fatalf("cache after nil replacement = %p, %d", retained, generation)
	}
	var empty *Graph
	if _, ok := empty.Field("app.Model0", "relation"); ok {
		t.Fatal("nil graph lookup succeeded")
	}
}

func TestValidateWireRejectsMissingRequiredMembers(t *testing.T) {
	payload, err := json.Marshal(syntheticSnapshot(1))
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(payload, &root); err != nil {
		t.Fatal(err)
	}
	delete(root, "schema_sources")
	missingTop, _ := json.Marshal(root)
	if err := ValidateWire(missingTop); err == nil {
		t.Fatal("missing schema_sources error = nil")
	}

	if err := json.Unmarshal(payload, &root); err != nil {
		t.Fatal(err)
	}
	apps := root["apps"].(map[string]any)
	models := apps["app"].(map[string]any)["models"].(map[string]any)
	fields := models["Model0"].(map[string]any)["fields"].(map[string]any)
	delete(fields["relation"].(map[string]any), "help_text")
	missingField, _ := json.Marshal(root)
	if err := ValidateWire(missingField); err == nil {
		t.Fatal("missing field help_text error = nil")
	}

	if err := json.Unmarshal(payload, &root); err != nil {
		t.Fatal(err)
	}
	root["apps"] = nil
	nullApps, _ := json.Marshal(root)
	if err := ValidateWire(nullApps); err == nil {
		t.Fatal("null apps error = nil")
	}

	if err := json.Unmarshal(payload, &root); err != nil {
		t.Fatal(err)
	}
	apps = root["apps"].(map[string]any)
	models = apps["app"].(map[string]any)["models"].(map[string]any)
	model := models["Model0"].(map[string]any)
	model["managed"] = nil
	nullManaged, _ := json.Marshal(root)
	if err := ValidateWire(nullManaged); err == nil {
		t.Fatal("null managed error = nil")
	}

	if err := json.Unmarshal(payload, &root); err != nil {
		t.Fatal(err)
	}
	apps = root["apps"].(map[string]any)
	models = apps["app"].(map[string]any)["models"].(map[string]any)
	model = models["Model0"].(map[string]any)
	start := model["source_range"].(map[string]any)["start"].(map[string]any)
	delete(start, "column")
	missingColumn, _ := json.Marshal(root)
	if err := ValidateWire(missingColumn); err == nil {
		t.Fatal("missing source column error = nil")
	}

	if err := json.Unmarshal(payload, &root); err != nil {
		t.Fatal(err)
	}
	apps = root["apps"].(map[string]any)
	models = apps["app"].(map[string]any)["models"].(map[string]any)
	model = models["Model0"].(map[string]any)
	model["indexes"] = []any{map[string]any{
		"name": "broken", "fields": []any{map[string]any{"order": "asc"}}, "expressions": []any{},
		"condition": nil, "include": []any{}, "opclasses": []any{}, "db_tablespace": nil,
		"source_range": model["source_range"],
	}}
	missingIndexField, _ := json.Marshal(root)
	if err := ValidateWire(missingIndexField); err == nil {
		t.Fatal("missing index field name error = nil")
	}

	if err := json.Unmarshal(payload, &root); err != nil {
		t.Fatal(err)
	}
	root["schema_sources"] = []any{nil}
	nullSource, _ := json.Marshal(root)
	if err := ValidateWire(nullSource); err == nil {
		t.Fatal("null schema source element error = nil")
	}
}

func TestBuildRejectsQueryAliasCollisionsDeterministically(t *testing.T) {
	for range 100 {
		snapshot := syntheticSnapshot(2)
		model := snapshot.Apps["app"].Models["Model0"]
		reverse := model.Fields["reverse"]
		collision := "relation"
		reverse.QueryName = &collision
		model.Fields["reverse"] = reverse
		snapshot.Apps["app"].Models["Model0"] = model
		if _, err := Build(snapshot); err == nil {
			t.Fatal("query alias collision error = nil")
		}
	}
}

func BenchmarkGraphLookup(b *testing.B) {
	for _, size := range []int{10, 1_000, 10_000} {
		graph, err := Build(syntheticSnapshot(size))
		if err != nil {
			b.Fatal(err)
		}
		label := fmt.Sprintf("app.Model%d", size-1)
		b.Run(fmt.Sprintf("models-%d", size), func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				field, ok := graph.Field(label, "relation")
				if !ok {
					b.Fatal("field not found")
				}
				if _, ok := field.RelatedModel(); !ok {
					b.Fatal("relation target not found")
				}
				if _, ok := graph.AttnameField(label, "relation_id"); !ok {
					b.Fatal("attname not found")
				}
				if _, ok := graph.QueryField(label, "relation"); !ok {
					b.Fatal("query path not found")
				}
				if _, ok := graph.AccessorField(label, "reverse_set"); !ok {
					b.Fatal("accessor not found")
				}
				if _, ok := graph.Manager(label, "objects"); !ok {
					b.Fatal("manager not found")
				}
			}
		})
	}
}

func syntheticSnapshot(count int) Snapshot {
	models := make(map[string]Model, count)
	for index := 0; index < count; index++ {
		name := fmt.Sprintf("Model%d", index)
		related := fmt.Sprintf("app.Model%d", (index+1)%count)
		attname := "relation_id"
		queryName := "related_" + fmt.Sprintf("model%d", index)
		accessorName := fmt.Sprintf("model%d_set", index)
		direction := "forward"
		reverseDirection := "reverse"
		reverseRelated := fmt.Sprintf("app.Model%d", (index+count-1)%count)
		reverseQueryName := "reverse"
		reverseAccessorName := "reverse_set"
		models[name] = Model{
			CanonicalLabel: "app." + name,
			Module:         "app.models",
			Qualname:       name,
			FilePath:       "/project/app/models.py",
			LineNumber:     index + 1,
			SourceRange:    testRange(index + 1),
			DefaultManager: "objects",
			BaseManager:    BaseManager{Name: "_base_manager", OwnerClass: "django.db.models.Manager"},
			Managers:       []Manager{{Name: "objects", OwnerClass: "django.db.models.Manager", SourceRange: testRange(index + 1)}},
			Fields: map[string]Field{
				"relation": {
					Type:              "django.db.models.ForeignKey",
					InternalType:      "ForeignKey",
					Name:              "relation",
					Attname:           &attname,
					IsRelation:        true,
					RelatedModel:      &related,
					QueryName:         &queryName,
					AccessorName:      &accessorName,
					SourceModel:       "app." + name,
					SourceRange:       testRange(index + 1),
					RelationDirection: &direction,
					LookupPaths:       []LookupPath{{Lookups: []string{"exact"}}},
				},
				"reverse": {
					Type:              "django.db.models.ManyToOneRel",
					InternalType:      "ManyToOneRel",
					Name:              "reverse",
					IsRelation:        true,
					RelatedModel:      &reverseRelated,
					QueryName:         &reverseQueryName,
					AccessorName:      &reverseAccessorName,
					SourceModel:       reverseRelated,
					SourceRange:       testRange(index + 1),
					RelationDirection: &reverseDirection,
					LookupPaths:       []LookupPath{{Lookups: []string{"exact"}}},
				},
			},
		}
	}
	return Snapshot{
		SchemaVersion:           Version,
		PositionEncoding:        PositionEncoding,
		LookupTransformMaxDepth: 2,
		LookupPathMaxCount:      512,
		Apps: map[string]App{
			"app": {
				Label:      "app",
				ImportName: "app",
				RootPath:   "/project/app",
				Models:     models,
			},
		},
	}
}

func testRange(line int) *SourceRange {
	return &SourceRange{
		FilePath: "/project/app/models.py",
		Start:    Position{Line: line},
		End:      Position{Line: line, Column: 1},
	}
}
