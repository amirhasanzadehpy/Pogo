package schema

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
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
	if label, ok := graph.CanonicalLabelForClass("app.models.Model1"); !ok || label != "app.Model1" {
		t.Fatalf("class path resolution = %q, %v", label, ok)
	}
	var queryNames []string
	graph.VisitQueryFields("app.Model1", func(access FieldAccess) bool {
		queryNames = append(queryNames, access.Name)
		return true
	})
	if got, want := fmt.Sprint(queryNames), "[relation relation_id reverse]"; got != want {
		t.Fatalf("query field order = %s, want %s", got, want)
	}
	var instanceNames []string
	graph.VisitInstanceFields("app.Model1", func(access FieldAccess) bool {
		instanceNames = append(instanceNames, access.Name)
		return true
	})
	if got, want := fmt.Sprint(instanceNames), "[relation relation_id reverse_set]"; got != want {
		t.Fatalf("instance field order = %s, want %s", got, want)
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

func TestBuildIndexesUniqueModelPackageExports(t *testing.T) {
	snapshot := syntheticSnapshot(2)
	model := snapshot.Apps["app"].Models["Model1"]
	model.Module = "app.models.nested"
	snapshot.Apps["app"].Models["Model1"] = model
	graph, err := Build(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if label, ok := graph.CanonicalLabelForClass("app.models.Model1"); !ok || label != "app.Model1" {
		t.Fatalf("model package export = %q, %v", label, ok)
	}

	duplicate := model
	duplicate.CanonicalLabel = "other.Model1"
	duplicate.Module = "app.models.other"
	duplicate.FilePath = syntheticPath("other_models.py")
	duplicate.LineNumber = 10
	duplicate.SourceRange = testRange(10)
	duplicate.SourceRange.FilePath = duplicate.FilePath
	for name, field := range duplicate.Fields {
		field.SourceModel = duplicate.CanonicalLabel
		field.RelatedModel = &duplicate.CanonicalLabel
		duplicate.Fields[name] = field
	}
	snapshot.Apps["other"] = App{Label: "other", ImportName: "app", RootPath: filepath.Dir(duplicate.FilePath), Models: map[string]Model{"Model1": duplicate}}
	graph, err = Build(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if label, ok := graph.CanonicalLabelForClass("app.models.Model1"); ok {
		t.Fatalf("ambiguous model package export resolved to %q", label)
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

func TestValidateWireRequiresSchemaSourcesCompleteness(t *testing.T) {
	payload, err := json.Marshal(syntheticSnapshot(1))
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(payload, &root); err != nil {
		t.Fatal(err)
	}
	delete(root, "schema_sources_complete")
	missing, _ := json.Marshal(root)
	if err := ValidateWire(missing); err == nil {
		t.Fatal("missing schema_sources_complete error = nil")
	}
	root["schema_sources_complete"] = "true"
	wrongKind, _ := json.Marshal(root)
	if err := ValidateWire(wrongKind); err == nil {
		t.Fatal("non-boolean schema_sources_complete error = nil")
	}
}

func TestValidateManagerWireRequiresQuerySetBindings(t *testing.T) {
	method := MethodKey{Name: "active", OwnerClass: "app.BaseQuerySet"}
	manager := Manager{
		Name: "objects", OwnerClass: "app.Manager", SourceRange: testRange(1), Methods: []Method{},
		QuerySetMethods: []BoundQuerySetMethod{{Method: method, AvailableOnManager: true}},
	}
	payload, err := json.Marshal(manager)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateManagerWire(payload); err != nil {
		t.Fatalf("valid manager wire rejected: %v", err)
	}
	var object map[string]any
	if err := json.Unmarshal(payload, &object); err != nil {
		t.Fatal(err)
	}
	delete(object, "queryset_methods")
	missing, _ := json.Marshal(object)
	if err := validateManagerWire(missing); err == nil {
		t.Fatal("missing queryset_methods error = nil")
	}
	if err := json.Unmarshal(payload, &object); err != nil {
		t.Fatal(err)
	}
	binding := object["queryset_methods"].([]any)[0].(map[string]any)
	delete(binding, "available_on_manager")
	missingFlag, _ := json.Marshal(object)
	if err := validateManagerWire(missingFlag); err == nil {
		t.Fatal("missing available_on_manager error = nil")
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

func TestRelationContextsUseDjangoNamesAndCardinality(t *testing.T) {
	snapshot := syntheticSnapshot(2)
	model := snapshot.Apps["app"].Models["Model0"]

	manyToOne := string(RelationManyToOne)
	forward := model.Fields["relation"]
	forward.RelationCardinality = &manyToOne
	model.Fields["relation"] = forward

	oneToMany := string(RelationOneToMany)
	reverseQueryName := "reverse_query"
	reverseAccessorName := "reverse_set"
	reverse := model.Fields["reverse"]
	reverse.QueryName = &reverseQueryName
	reverse.AccessorName = &reverseAccessorName
	reverse.RelationCardinality = &oneToMany
	model.Fields["reverse"] = reverse

	reverseDirection := string(RelationReverse)
	oneToOne := string(RelationOneToOne)
	reverseOneQueryName := "reverse_one_query"
	reverseOneAccessorName := "reverse_one_accessor"
	related := "app.Model1"
	model.Fields["reverse_one"] = Field{
		Type:                "django.db.models.OneToOneRel",
		InternalType:        "OneToOneRel",
		Name:                "reverse_one",
		IsRelation:          true,
		RelatedModel:        &related,
		QueryName:           &reverseOneQueryName,
		AccessorName:        &reverseOneAccessorName,
		SourceModel:         related,
		SourceRange:         testRange(1),
		RelationDirection:   &reverseDirection,
		RelationCardinality: &oneToOne,
	}

	forwardDirection := string(RelationForward)
	manyToMany := string(RelationManyToMany)
	model.Fields["stores"] = Field{
		Type:                "django.db.models.ManyToManyField",
		InternalType:        "ManyToManyField",
		Name:                "stores",
		IsRelation:          true,
		RelatedModel:        &related,
		SourceModel:         "app.Model0",
		SourceRange:         testRange(1),
		RelationDirection:   &forwardDirection,
		RelationCardinality: &manyToMany,
	}
	snapshot.Apps["app"].Models["Model0"] = model

	graph, err := Build(snapshot)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		visit   func(func(FieldAccess) bool) bool
		want    string
		present []string
		absent  []string
		lookup  func(string) (FieldAccess, bool)
	}{
		{
			name:    "query",
			visit:   func(visitor func(FieldAccess) bool) bool { return graph.VisitQueryRelations("app.Model0", visitor) },
			want:    "[relation stores relation_id reverse_one_query reverse_query]",
			present: []string{"relation", "relation_id", "reverse_query", "reverse_one_query", "stores"},
			absent:  []string{"reverse_set", "reverse_one_accessor"},
			lookup:  func(name string) (FieldAccess, bool) { return graph.QueryRelation("app.Model0", name) },
		},
		{
			name: "select_related",
			visit: func(visitor func(FieldAccess) bool) bool {
				return graph.VisitSelectRelatedRelations("app.Model0", visitor)
			},
			want:    "[relation reverse_one_query]",
			present: []string{"relation", "reverse_one_query"},
			absent:  []string{"relation_id", "reverse_query", "reverse_one_accessor", "stores"},
			lookup:  func(name string) (FieldAccess, bool) { return graph.SelectRelatedRelation("app.Model0", name) },
		},
		{
			name: "prefetch_related",
			visit: func(visitor func(FieldAccess) bool) bool {
				return graph.VisitPrefetchRelatedRelations("app.Model0", visitor)
			},
			want:    "[relation stores reverse_one_accessor reverse_set]",
			present: []string{"relation", "stores", "reverse_set", "reverse_one_accessor"},
			absent:  []string{"relation_id", "reverse_query", "reverse_one_query"},
			lookup:  func(name string) (FieldAccess, bool) { return graph.PrefetchRelatedRelation("app.Model0", name) },
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var names []string
			if !test.visit(func(access FieldAccess) bool {
				names = append(names, access.Name)
				return true
			}) {
				t.Fatal("visitor rejected a known model")
			}
			if got := fmt.Sprint(names); got != test.want {
				t.Fatalf("visitor names = %s, want %s", got, test.want)
			}
			for _, name := range test.present {
				access, ok := test.lookup(name)
				if !ok || access.Field == nil || !access.Field.IsRelation() {
					t.Fatalf("lookup %q = %#v, %v", name, access, ok)
				}
			}
			for _, name := range test.absent {
				if _, ok := test.lookup(name); ok {
					t.Fatalf("lookup %q unexpectedly succeeded", name)
				}
			}
		})
	}

	access, ok := graph.QueryRelation("app.Model0", "reverse_query")
	if !ok {
		t.Fatal("reverse query relation is missing")
	}
	if direction, ok := access.Field.RelationDirection(); !ok || direction != RelationReverse {
		t.Fatalf("direction = %q, %v", direction, ok)
	}
	if cardinality, ok := access.Field.RelationCardinality(); !ok || cardinality != RelationOneToMany {
		t.Fatalf("cardinality = %q, %v", cardinality, ok)
	}
	if accessor, ok := access.Field.AccessorName(); !ok || accessor != "reverse_set" {
		t.Fatalf("accessor = %q, %v", accessor, ok)
	}
	forwardAccess, _ := graph.QueryRelation("app.Model0", "relation")
	if attname, ok := forwardAccess.Field.Attname(); !ok || attname != "relation_id" {
		t.Fatalf("attname = %q, %v", attname, ok)
	}
}

func TestLookupMetadataReturnsDeepCopies(t *testing.T) {
	snapshot := syntheticSnapshot(1)
	model := snapshot.Apps["app"].Models["Model0"]
	field := model.Fields["relation"]
	field.Lookups = []string{"exact", "isnull"}
	field.UnsupportedLookups = []string{"contains"}
	field.Transforms = []string{"date"}
	field.LookupPaths = []LookupPath{
		{Lookups: []string{"exact", "isnull"}},
		{Transforms: []string{"date"}, Kinds: []string{"transform"}, Lookups: []string{"gte"}},
	}
	field.LookupPathsTruncated = true
	model.Fields["relation"] = field
	snapshot.Apps["app"].Models["Model0"] = model

	graph, err := Build(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	ref, ok := graph.Field("app.Model0", "relation")
	if !ok {
		t.Fatal("field is missing")
	}

	field.Lookups[0] = "mutated-input"
	field.LookupPaths[1].Transforms[0] = "mutated-input"
	paths := ref.LookupPaths()
	paths[0].Lookups[0] = "mutated-result"
	paths[1].Transforms[0] = "mutated-result"
	lookups := ref.Lookups()
	lookups[0] = "mutated-result"
	transforms := ref.Transforms()
	transforms[0] = "mutated-result"
	unsupported := ref.UnsupportedLookups()
	unsupported[0] = "mutated-result"
	ref.VisitLookupPaths(func(path LookupPath) bool {
		if len(path.Lookups) > 0 {
			path.Lookups[0] = "mutated-visitor"
		}
		return true
	})

	paths = ref.LookupPaths()
	if got, want := fmt.Sprint(ref.Lookups()), "[exact isnull]"; got != want {
		t.Fatalf("lookups = %s, want %s", got, want)
	}
	if got, want := fmt.Sprint(ref.UnsupportedLookups()), "[contains]"; got != want {
		t.Fatalf("unsupported lookups = %s, want %s", got, want)
	}
	if got, want := fmt.Sprint(ref.Transforms()), "[date]"; got != want {
		t.Fatalf("transforms = %s, want %s", got, want)
	}
	if got, want := fmt.Sprint(paths[0].Lookups), "[exact isnull]"; got != want {
		t.Fatalf("root path lookups = %s, want %s", got, want)
	}
	if got, want := fmt.Sprint(paths[1].Transforms), "[date]"; got != want {
		t.Fatalf("path transforms = %s, want %s", got, want)
	}
	if !ref.LookupPathsTruncated() {
		t.Fatal("lookup path truncation flag was lost")
	}
}

func TestManagerMethodsAttachToExactQuerySetClass(t *testing.T) {
	snapshot := syntheticSnapshot(1)
	model := snapshot.Apps["app"].Models["Model0"]
	liveClass := "app.query.LiveQuerySet"
	baseClass := "app.query.BaseQuerySet"
	archiveClass := "app.query.ArchiveQuerySet"
	liveSignature := "(*, enabled=True)"
	liveDocstring := "Return live rows."
	managerSignature := "(limit=10)"
	managerDocstring := "Return featured rows."
	methodDefs := []Method{
		{
			Name:             "active",
			OwnerClass:       baseClass,
			Signature:        &liveSignature,
			Docstring:        &liveDocstring,
			SourceRange:      testRange(10),
			Chainable:        true,
			AssumedChainable: true,
		},
		{Name: "hidden", OwnerClass: baseClass, SourceRange: testRange(15), Chainable: true},
		{Name: "archived", OwnerClass: archiveClass, SourceRange: testRange(20), Chainable: true},
	}
	snapshot.QuerySetMethodDefs = methodDefs
	model.QuerySetMethods = []MethodKey{
		{Name: methodDefs[0].Name, OwnerClass: methodDefs[0].OwnerClass},
		{Name: methodDefs[1].Name, OwnerClass: methodDefs[1].OwnerClass},
		{Name: methodDefs[2].Name, OwnerClass: methodDefs[2].OwnerClass},
	}
	model.Managers = []Manager{
		{
			Name:          "archive",
			OwnerClass:    "app.manager.ArchiveManager",
			QuerySetClass: &archiveClass,
			SourceRange:   testRange(4),
		},
		{
			Name:          "objects",
			OwnerClass:    "app.manager.LiveManager",
			QuerySetClass: &liveClass,
			Default:       true,
			Local:         true,
			SourceRange:   testRange(3),
			QuerySetMethods: []BoundQuerySetMethod{
				{Method: model.QuerySetMethods[0], AvailableOnManager: true},
				{Method: model.QuerySetMethods[1]},
			},
			Methods: []Method{{
				Name:        "featured",
				OwnerClass:  "app.manager.LiveManager",
				Signature:   &managerSignature,
				Docstring:   &managerDocstring,
				SourceRange: testRange(30),
				Chainable:   true,
			}},
		},
		{
			Name:          "replica",
			OwnerClass:    "app.manager.LiveManager",
			QuerySetClass: &liveClass,
			SourceRange:   testRange(5),
			QuerySetMethods: []BoundQuerySetMethod{
				{Method: model.QuerySetMethods[0], AvailableOnManager: true},
			},
		},
	}
	model.DefaultManager = "objects"
	model.CustomManagers = []string{"archive", "objects", "replica"}
	snapshot.Apps["app"].Models["Model0"] = model

	graph, err := Build(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	objects, _ := graph.Manager("app.Model0", "objects")
	replica, _ := graph.Manager("app.Model0", "replica")
	archive, _ := graph.Manager("app.Model0", "archive")
	if class, ok := objects.QuerySetClass(); !ok || class != liveClass {
		t.Fatalf("objects QuerySet class = %q, %v", class, ok)
	}
	live, ok := objects.QuerySet()
	if !ok {
		t.Fatal("objects QuerySet is missing")
	}
	replicaQuerySet, ok := replica.QuerySet()
	if !ok || replicaQuerySet != live {
		t.Fatal("managers with the same exact QuerySet class do not share identity")
	}
	indexedLive, ok := graph.QuerySet("app.Model0", liveClass)
	if !ok || indexedLive != live {
		t.Fatal("model QuerySet index does not preserve manager identity")
	}
	active, ok := live.Method("active")
	if !ok || !active.Chainable() || !active.AssumedChainable() {
		t.Fatal("live QuerySet active method metadata is incomplete")
	}
	if signature, ok := active.Signature(); !ok || signature != liveSignature {
		t.Fatalf("active signature = %q, %v", signature, ok)
	}
	if docstring, ok := active.Docstring(); !ok || docstring != liveDocstring {
		t.Fatalf("active docstring = %q, %v", docstring, ok)
	}
	if _, ok := live.Method("archived"); ok {
		t.Fatal("archive-specific method attached to live QuerySet")
	}
	if _, ok := live.Method("hidden"); !ok {
		t.Fatal("inherited queryset-only method is not attached to concrete QuerySet")
	}
	if _, ok := objects.QuerySetMethod("active"); !ok {
		t.Fatal("manager-visible inherited QuerySet method is missing")
	}
	if _, ok := objects.QuerySetMethod("hidden"); ok {
		t.Fatal("queryset-only method leaked onto manager")
	}
	archiveQuerySet, ok := archive.QuerySet()
	if !ok {
		t.Fatal("archive QuerySet is missing")
	}
	if _, ok := archiveQuerySet.Method("archived"); !ok {
		t.Fatal("archive method is not attached")
	}
	if _, ok := archiveQuerySet.Method("active"); ok {
		t.Fatal("live-specific method attached to archive QuerySet")
	}
	featured, ok := objects.Method("featured")
	if !ok || !featured.Chainable() {
		t.Fatal("manager method is not attached")
	}
	if signature, ok := featured.Signature(); !ok || signature != managerSignature {
		t.Fatalf("featured signature = %q, %v", signature, ok)
	}
	if _, ok := archive.Method("featured"); ok {
		t.Fatal("manager-specific method leaked to another manager")
	}

	var managers []string
	graph.VisitManagers("app.Model0", func(manager *ManagerRef) bool {
		managers = append(managers, manager.Name())
		return true
	})
	if got, want := fmt.Sprint(managers), "[archive objects replica]"; got != want {
		t.Fatalf("manager order = %s, want %s", got, want)
	}
	var methods []string
	graph.VisitQuerySetMethods("app.Model0", func(method *MethodRef) bool {
		methods = append(methods, method.Name()+":"+method.OwnerClass())
		return true
	})
	if got, want := fmt.Sprint(methods), "[active:app.query.BaseQuerySet archived:app.query.ArchiveQuerySet hidden:app.query.BaseQuerySet]"; got != want {
		t.Fatalf("QuerySet method order = %s, want %s", got, want)
	}

	liveSignature = "mutated"
	liveDocstring = "mutated"
	if signature, _ := active.Signature(); signature != "(*, enabled=True)" {
		t.Fatalf("frozen signature = %q", signature)
	}
	if docstring, _ := active.Docstring(); docstring != "Return live rows." {
		t.Fatalf("frozen docstring = %q", docstring)
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

func BenchmarkGraphBuild(b *testing.B) {
	for _, size := range []int{10, 1_000, 10_000} {
		snapshot := syntheticSnapshot(size)
		b.Run(fmt.Sprintf("models-%d", size), func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				if _, err := Build(snapshot); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
	frame := syntheticSnapshot(1)
	model := frame.Apps["app"].Models["Model0"]
	for index := 0; index < 256; index++ {
		name := fmt.Sprintf("dense_%03d", index)
		model.Fields[name] = Field{Type: "django.db.models.CharField", InternalType: "CharField", Name: name, SourceModel: "app.Model0", SourceRange: testRange(index + 2), LookupPaths: []LookupPath{{Lookups: []string{"exact"}}}}
	}
	frame.Apps["app"].Models["Model0"] = model
	b.Run("dense-relations-256", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			if _, err := Build(frame); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkCacheSnapshots(b *testing.B) {
	first, err := Build(syntheticSnapshot(1_000))
	if err != nil {
		b.Fatal(err)
	}
	second, err := Build(syntheticSnapshot(1_001))
	if err != nil {
		b.Fatal(err)
	}
	cache := &Cache{}
	cache.Replace(first)
	b.Run("read", func(b *testing.B) {
		b.ReportAllocs()
		totals := make([]time.Duration, b.N)
		b.ResetTimer()
		for index := 0; index < b.N; index++ {
			started := time.Now()
			graph, generation := cache.Load()
			if graph == nil || generation == 0 {
				b.Fatal("empty cache snapshot")
			}
			totals[index] = time.Since(started)
		}
		b.StopTimer()
		reportSchemaPercentiles(b, totals)
	})
	b.Run("parallel-read", func(b *testing.B) {
		b.ReportAllocs()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				graph, generation := cache.Load()
				if graph == nil || generation == 0 {
					b.Fatal("empty cache snapshot")
				}
			}
		})
	})
	b.Run("swap", func(b *testing.B) {
		b.ReportAllocs()
		totals := make([]time.Duration, b.N)
		b.ResetTimer()
		for index := 0; index < b.N; index++ {
			started := time.Now()
			if index%2 == 0 {
				cache.Replace(first)
			} else {
				cache.Replace(second)
			}
			totals[index] = time.Since(started)
		}
		b.StopTimer()
		reportSchemaPercentiles(b, totals)
	})
	b.Run("concurrent-swap", func(b *testing.B) {
		b.ReportAllocs()
		stop := make(chan struct{})
		var readers sync.WaitGroup
		for range runtime.GOMAXPROCS(0) {
			readers.Add(1)
			go func() {
				defer readers.Done()
				for {
					select {
					case <-stop:
						return
					default:
						graph, generation := cache.Load()
						if graph == nil || generation == 0 {
							b.Error("empty concurrent snapshot")
							return
						}
					}
				}
			}()
		}
		b.ResetTimer()
		totals := make([]time.Duration, b.N)
		for index := 0; index < b.N; index++ {
			started := time.Now()
			if index%2 == 0 {
				cache.Replace(first)
			} else {
				cache.Replace(second)
			}
			totals[index] = time.Since(started)
		}
		b.StopTimer()
		close(stop)
		readers.Wait()
		reportSchemaPercentiles(b, totals)
	})
}

func reportSchemaPercentiles(b *testing.B, values []time.Duration) {
	b.Helper()
	sort.Slice(values, func(left, right int) bool { return values[left] < values[right] })
	for _, percent := range []int{50, 95, 99} {
		index := (len(values)*percent + 99) / 100
		if index > 0 {
			index--
		}
		b.ReportMetric(float64(values[index].Nanoseconds())/1000, fmt.Sprintf("p%d-us", percent))
	}
}

func TestEffectivePrimaryKeyOwnsPKAlias(t *testing.T) {
	snapshot := syntheticSnapshot(2)
	parent := "app.Model1"
	direction, cardinality := "forward", "many-to-one"
	child := snapshot.Apps["app"].Models["Model0"]
	child.Fields = map[string]Field{
		"id": {
			Type: "django.db.models.BigAutoField", InternalType: "BigAutoField", Name: "id", SourceModel: "app.Model1",
			PrimaryKey: true, EffectivePrimaryKey: false, SourceRange: testRange(1),
		},
		"publication_ptr": {
			Type: "django.db.models.OneToOneField", InternalType: "OneToOneField", Name: "publication_ptr", SourceModel: "app.Model0",
			PrimaryKey: true, EffectivePrimaryKey: true, IsRelation: true, RelatedModel: &parent,
			RelationDirection: &direction, RelationCardinality: &cardinality, SourceRange: testRange(1),
		},
	}
	snapshot.Apps["app"].Models["Model0"] = child
	graph, err := Build(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	access, ok := graph.QueryAccess("app.Model0", "pk")
	if !ok || access.Field.Name() != "publication_ptr" || !access.Field.IsRelation() {
		t.Fatalf("pk access = %#v, %v", access, ok)
	}
}

func TestNavigationSourceAccessors(t *testing.T) {
	graph, err := Build(syntheticSnapshot(2))
	if err != nil {
		t.Fatal(err)
	}
	modelRange, ok := graph.ModelSourceRange("app.Model0")
	if !ok || modelRange != *testRange(1) {
		t.Fatalf("ModelSourceRange() = %#v, %v", modelRange, ok)
	}
	field, ok := graph.Field("app.Model0", "relation")
	if !ok {
		t.Fatal("relation field not found")
	}
	fieldRange, ok := field.SourceRange()
	if !ok || fieldRange != *testRange(1) {
		t.Fatalf("FieldRef.SourceRange() = %#v, %v", fieldRange, ok)
	}
	manager, ok := graph.Manager("app.Model0", "objects")
	if !ok {
		t.Fatal("objects manager not found")
	}
	managerRange, ok := manager.SourceRange()
	if !ok || managerRange != *testRange(1) {
		t.Fatalf("ManagerRef.SourceRange() = %#v, %v", managerRange, ok)
	}
	if _, ok := (*FieldRef)(nil).SourceRange(); ok {
		t.Fatal("nil field returned a source range")
	}
	if _, ok := (*ManagerRef)(nil).SourceRange(); ok {
		t.Fatal("nil manager returned a source range")
	}
}

func TestBuildRejectsEmptySourceDigest(t *testing.T) {
	snapshot := syntheticSnapshot(1)
	model := snapshot.Apps["app"].Models["Model0"]
	model.SourceRange.SourceDigest = ""
	snapshot.Apps["app"].Models["Model0"] = model
	if _, err := Build(snapshot); err == nil || !strings.Contains(err.Error(), "source digest") {
		t.Fatalf("Build() error = %v, want invalid source digest", err)
	}
}

func TestRelationFieldForInheritedSourceRequiresOneTarget(t *testing.T) {
	snapshot := syntheticSnapshot(2)
	oneTarget := "app.Model1"
	for name, model := range snapshot.Apps["app"].Models {
		field := model.Fields["relation"]
		field.SourceModel = "app.AbstractRelation"
		field.SourceModelAbstract = true
		field.RelatedModel = &oneTarget
		field.SourceRange = testRange(1)
		model.Fields["relation"] = field
		snapshot.Apps["app"].Models[name] = model
	}
	graph, err := Build(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	field, ok := graph.RelationFieldForSource(syntheticPath("models.py"), "AbstractRelation", "relation", Position{Line: 1}, Position{Line: 1, Column: 1})
	if !ok || field.SourceModel() != "app.AbstractRelation" {
		t.Fatalf("RelationFieldForSource() = %#v, %v", field, ok)
	}

	model := snapshot.Apps["app"].Models["Model0"]
	fieldDTO := model.Fields["relation"]
	otherTarget := "app.Model0"
	fieldDTO.RelatedModel = &otherTarget
	model.Fields["relation"] = fieldDTO
	snapshot.Apps["app"].Models["Model0"] = model
	graph, err = Build(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if field, ok := graph.RelationFieldForSource(syntheticPath("models.py"), "AbstractRelation", "relation", Position{Line: 1}, Position{Line: 1, Column: 1}); ok {
		t.Fatalf("ambiguous RelationFieldForSource() = %#v", field)
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
			FilePath:       syntheticPath("models.py"),
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
				RootPath:   syntheticPath(),
				Models:     models,
			},
		},
	}
}

func syntheticPath(elements ...string) string {
	parts := append([]string{os.TempDir(), "pogo-tests", "project", "app"}, elements...)
	return filepath.Join(parts...)
}

func testRange(line int) *SourceRange {
	return &SourceRange{
		FilePath:     syntheticPath("models.py"),
		SourceDigest: strings.Repeat("0", 64),
		Start:        Position{Line: line},
		End:          Position{Line: line, Column: 1},
	}
}
