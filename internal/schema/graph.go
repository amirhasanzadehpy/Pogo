package schema

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

func MatchesSourceDigest(source []byte, digest string) bool {
	if len(digest) != 64 {
		return false
	}
	sum := sha256.Sum256(source)
	return hex.EncodeToString(sum[:]) == digest
}

const (
	Version          = 1
	PositionEncoding = "utf-8-bytes"
	maxLookupDepth   = 2
	maxLookupPaths   = 512
)

type Graph struct {
	snapshot    Snapshot
	canonical   map[string]*modelIndex
	classPath   map[string]*modelIndex
	sourceClass map[string]*modelIndex
	module      map[string][]*modelIndex
}

type modelIndex struct {
	name           string
	model          Model
	fields         map[string]*FieldRef
	attnames       map[string]*FieldRef
	queries        map[string]*FieldRef
	accessors      map[string]*FieldRef
	relations      [relationContextCount]relationIndex
	managers       map[string]*ManagerRef
	querySets      map[string]*QuerySetRef
	queryAccess    []FieldAccess
	instanceAccess []FieldAccess
	managerOrder   []*ManagerRef
	querySetOrder  []*QuerySetRef
	methodOrder    []*MethodRef
	queryByName    map[string]FieldAccess
	instanceByName map[string]FieldAccess
}

type FieldRef struct {
	field   Field
	related *modelIndex
}

type ManagerRef struct {
	manager             Manager
	methods             map[string]*MethodRef
	methodOrder         []*MethodRef
	querySet            *QuerySetRef
	querySetMethods     map[string]*MethodRef
	querySetMethodOrder []*MethodRef
}

type QuerySetRef struct {
	class       string
	methods     map[string]*MethodRef
	methodOrder []*MethodRef
}

type MethodRef struct {
	method Method
}

type RelationDirection string

const (
	RelationForward RelationDirection = "forward"
	RelationReverse RelationDirection = "reverse"
)

type RelationCardinality string

const (
	RelationManyToOne  RelationCardinality = "many-to-one"
	RelationOneToMany  RelationCardinality = "one-to-many"
	RelationOneToOne   RelationCardinality = "one-to-one"
	RelationManyToMany RelationCardinality = "many-to-many"
)

type RelationContext uint8

const (
	RelationQuery RelationContext = iota
	RelationSelectRelated
	RelationPrefetchRelated
	relationContextCount
)

type relationIndex struct {
	byName  map[string]FieldAccess
	ordered []FieldAccess
}

type FieldAccessKind uint8

const (
	FieldAccessDeclared FieldAccessKind = iota
	FieldAccessAttname
	FieldAccessReverse
)

type FieldAccess struct {
	Name  string
	Kind  FieldAccessKind
	Field *FieldRef
}

type ModelInfo struct {
	CanonicalLabel    string
	FilePath          string
	Managed           bool
	HasAbstractParent bool
	MultiTableChild   bool
	IndexCount        int
	ConstraintCount   int
}

func Build(snapshot Snapshot) (*Graph, error) {
	cloned, err := cloneSnapshot(snapshot)
	if err != nil {
		return nil, err
	}
	snapshot = cloned
	if snapshot.SchemaVersion != Version {
		return nil, fmt.Errorf("schema version %d, want %d", snapshot.SchemaVersion, Version)
	}
	if snapshot.PositionEncoding != PositionEncoding {
		return nil, fmt.Errorf("position encoding %q, want %q", snapshot.PositionEncoding, PositionEncoding)
	}
	if snapshot.LookupTransformMaxDepth != maxLookupDepth || snapshot.LookupPathMaxCount != maxLookupPaths {
		return nil, errors.New("invalid lookup bounds")
	}

	graph := &Graph{
		snapshot:    snapshot,
		canonical:   make(map[string]*modelIndex),
		classPath:   make(map[string]*modelIndex),
		sourceClass: make(map[string]*modelIndex),
		module:      make(map[string][]*modelIndex),
	}
	for appKey, app := range snapshot.Apps {
		if appKey == "" || app.Label != appKey || app.ImportName == "" || app.RootPath == "" || !filepath.IsAbs(app.RootPath) {
			return nil, fmt.Errorf("invalid app metadata for key %q", appKey)
		}
		for modelName, model := range app.Models {
			if err := validateModel(appKey, modelName, model, snapshot); err != nil {
				return nil, err
			}
			if _, exists := graph.canonical[model.CanonicalLabel]; exists {
				return nil, fmt.Errorf("duplicate canonical model label %q", model.CanonicalLabel)
			}
			classPath := model.Module + "." + model.Qualname
			if _, exists := graph.classPath[classPath]; exists {
				return nil, fmt.Errorf("duplicate model class path %q", classPath)
			}
			index, err := buildModelIndex(modelName, model)
			if err != nil {
				return nil, err
			}
			graph.canonical[model.CanonicalLabel] = index
			graph.classPath[classPath] = index
			sourcePaths := []string{model.FilePath}
			if resolved := resolvedPath(model.FilePath); !samePath(filepath.Clean(model.FilePath), resolved) {
				sourcePaths = append(sourcePaths, resolved)
			}
			for _, sourcePath := range sourcePaths {
				sourceClass := sourceClassKey(sourcePath, model.Qualname)
				if existing := graph.sourceClass[sourceClass]; existing != nil && existing != index {
					return nil, fmt.Errorf("duplicate model source class %s in %q", model.Qualname, model.FilePath)
				}
				graph.sourceClass[sourceClass] = index
			}
			graph.module[model.Module] = append(graph.module[model.Module], index)
		}
	}
	for _, source := range snapshot.SchemaSources {
		if source == "" || !filepath.IsAbs(source) {
			return nil, fmt.Errorf("invalid schema source %q", source)
		}
	}

	for _, index := range graph.canonical {
		for fieldName, field := range index.fields {
			if !field.field.SourceModelAbstract {
				if _, exists := graph.canonical[field.field.SourceModel]; !exists {
					return nil, fmt.Errorf("field %s.%s has dangling source model %q", index.model.CanonicalLabel, fieldName, field.field.SourceModel)
				}
			}
			if field.field.RelatedModel != nil {
				related, exists := graph.canonical[*field.field.RelatedModel]
				if !exists {
					return nil, fmt.Errorf("field %s.%s has dangling related model %q", index.model.CanonicalLabel, fieldName, *field.field.RelatedModel)
				}
				field.related = related
			}
		}
		for _, parent := range index.model.Parents {
			if !parent.Abstract {
				if _, exists := graph.canonical[parent.CanonicalLabel]; !exists {
					return nil, fmt.Errorf("model %s has dangling parent %q", index.model.CanonicalLabel, parent.CanonicalLabel)
				}
			}
		}
	}
	return graph, nil
}

func (graph *Graph) SchemaAffectingPath(path string) bool {
	if graph == nil || strings.ToLower(filepath.Ext(path)) != ".py" {
		return false
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	absolute = resolvedPath(absolute)
	for _, source := range graph.snapshot.SchemaSources {
		if samePath(absolute, resolvedPath(source)) {
			return true
		}
	}
	for _, app := range graph.snapshot.Apps {
		if pathWithin(resolvedPath(app.RootPath), absolute) {
			return true
		}
	}
	return false
}

func resolvedPath(path string) string {
	path = filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return filepath.Clean(resolved)
	}
	ancestor := path
	var suffix []string
	for {
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return path
		}
		suffix = append([]string{filepath.Base(ancestor)}, suffix...)
		ancestor = parent
		if resolved, err := filepath.EvalSymlinks(ancestor); err == nil {
			parts := append([]string{resolved}, suffix...)
			return filepath.Clean(filepath.Join(parts...))
		}
	}
}

func samePath(left, right string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}

func sourceClassKey(filePath, qualname string) string {
	filePath = filepath.Clean(filePath)
	if runtime.GOOS == "windows" {
		filePath = strings.ToLower(filePath)
	}
	return filePath + "\x00" + qualname
}

func pathWithin(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	if err != nil || relative == ".." || filepath.IsAbs(relative) {
		return false
	}
	return relative != "" && relative != "." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) || relative == "."
}

func validateModel(appKey, modelName string, model Model, snapshot Snapshot) error {
	if modelName == "" || model.CanonicalLabel != appKey+"."+modelName {
		return fmt.Errorf("model key %q does not match canonical label %q", modelName, model.CanonicalLabel)
	}
	if model.Module == "" || model.Qualname == "" || model.FilePath == "" || model.LineNumber <= 0 {
		return fmt.Errorf("model %s has incomplete source metadata", model.CanonicalLabel)
	}
	if err := validateRange(model.SourceRange); err != nil {
		return fmt.Errorf("model %s source range: %w", model.CanonicalLabel, err)
	}
	if model.FilePath != model.SourceRange.FilePath || model.LineNumber != model.SourceRange.Start.Line {
		return fmt.Errorf("model %s has inconsistent source metadata", model.CanonicalLabel)
	}
	managerNames := make(map[string]struct{}, len(model.Managers))
	for _, manager := range model.Managers {
		if manager.Name == "" || manager.OwnerClass == "" || manager.SourceRange == nil {
			return fmt.Errorf("model %s has invalid manager", model.CanonicalLabel)
		}
		if _, exists := managerNames[manager.Name]; exists {
			return fmt.Errorf("model %s has duplicate manager %q", model.CanonicalLabel, manager.Name)
		}
		managerNames[manager.Name] = struct{}{}
		if err := validateRange(manager.SourceRange); err != nil {
			return fmt.Errorf("model %s manager %s source range: %w", model.CanonicalLabel, manager.Name, err)
		}
		if manager.QuerySetClass != nil && *manager.QuerySetClass == "" {
			return fmt.Errorf("model %s manager %s has empty QuerySet class", model.CanonicalLabel, manager.Name)
		}
		methodNames := make(map[string]struct{}, len(manager.Methods))
		for _, method := range manager.Methods {
			if err := validateMethod(method); err != nil {
				return fmt.Errorf("model %s manager %s method %s: %w", model.CanonicalLabel, manager.Name, method.Name, err)
			}
			if _, exists := methodNames[method.Name]; exists {
				return fmt.Errorf("model %s manager %s has duplicate method %q", model.CanonicalLabel, manager.Name, method.Name)
			}
			methodNames[method.Name] = struct{}{}
		}
		if len(manager.QuerySetMethods) > 0 && manager.QuerySetClass == nil {
			return fmt.Errorf("model %s manager %s has QuerySet methods without a QuerySet class", model.CanonicalLabel, manager.Name)
		}
		querySetMethodNames := make(map[string]struct{}, len(manager.QuerySetMethods))
		for _, binding := range manager.QuerySetMethods {
			if err := validateMethod(binding.Method); err != nil {
				return fmt.Errorf("model %s manager %s QuerySet method %s: %w", model.CanonicalLabel, manager.Name, binding.Method.Name, err)
			}
			if _, exists := querySetMethodNames[binding.Method.Name]; exists {
				return fmt.Errorf("model %s manager %s has duplicate QuerySet method %q", model.CanonicalLabel, manager.Name, binding.Method.Name)
			}
			querySetMethodNames[binding.Method.Name] = struct{}{}
		}
	}
	if _, exists := managerNames[model.DefaultManager]; !exists {
		return fmt.Errorf("model %s default manager %q is not indexed", model.CanonicalLabel, model.DefaultManager)
	}
	for _, customManager := range model.CustomManagers {
		if _, exists := managerNames[customManager]; !exists {
			return fmt.Errorf("model %s custom manager %q is not indexed", model.CanonicalLabel, customManager)
		}
	}
	for fieldName, field := range model.Fields {
		if fieldName == "" || field.Name != fieldName || field.Type == "" || field.InternalType == "" || field.SourceModel == "" {
			return fmt.Errorf("model %s has invalid field key %q", model.CanonicalLabel, fieldName)
		}
		if err := validateRange(field.SourceRange); err != nil {
			return fmt.Errorf("field %s.%s source range: %w", model.CanonicalLabel, fieldName, err)
		}
		if len(field.LookupPaths) > snapshot.LookupPathMaxCount {
			return fmt.Errorf("field %s.%s exceeds lookup path bound", model.CanonicalLabel, fieldName)
		}
		for _, path := range field.LookupPaths {
			if len(path.Transforms) != len(path.Kinds) || len(path.Transforms) > snapshot.LookupTransformMaxDepth {
				return fmt.Errorf("field %s.%s has invalid lookup path", model.CanonicalLabel, fieldName)
			}
			for _, kind := range path.Kinds {
				if kind != "transform" && kind != "key_transform" {
					return fmt.Errorf("field %s.%s has invalid lookup path kind %q", model.CanonicalLabel, fieldName, kind)
				}
			}
		}
		if field.IsRelation {
			if field.RelationDirection == nil || (*field.RelationDirection != "forward" && *field.RelationDirection != "reverse") {
				return fmt.Errorf("field %s.%s has invalid relation direction", model.CanonicalLabel, fieldName)
			}
			if field.RelationCardinality != nil {
				switch *field.RelationCardinality {
				case "many-to-one", "one-to-many", "one-to-one", "many-to-many":
				default:
					return fmt.Errorf("field %s.%s has invalid relation cardinality", model.CanonicalLabel, fieldName)
				}
			}
		} else if field.RelatedModel != nil || field.RelationDirection != nil || field.RelationCardinality != nil {
			return fmt.Errorf("field %s.%s has contradictory relation metadata", model.CanonicalLabel, fieldName)
		}
	}
	for _, parent := range model.Parents {
		if parent.CanonicalLabel == "" || parent.ClassPath == "" || parent.SourceRange == nil {
			return fmt.Errorf("model %s has invalid parent metadata", model.CanonicalLabel)
		}
		if err := validateRange(parent.SourceRange); err != nil {
			return fmt.Errorf("model %s parent %s source range: %w", model.CanonicalLabel, parent.CanonicalLabel, err)
		}
	}
	if model.BaseManager.Name == "" || model.BaseManager.OwnerClass == "" {
		return fmt.Errorf("model %s has invalid base manager", model.CanonicalLabel)
	}
	querySetMethods := make(map[string]struct{}, len(model.QuerySetMethods))
	for _, method := range model.QuerySetMethods {
		if err := validateMethod(method); err != nil {
			return fmt.Errorf("model %s QuerySet method %s: %w", model.CanonicalLabel, method.Name, err)
		}
		key := method.OwnerClass + "\x00" + method.Name
		if _, exists := querySetMethods[key]; exists {
			return fmt.Errorf("model %s has duplicate QuerySet method %s.%s", model.CanonicalLabel, method.OwnerClass, method.Name)
		}
		querySetMethods[key] = struct{}{}
	}
	for _, index := range model.Indexes {
		if index.Name == "" || index.SourceRange == nil {
			return fmt.Errorf("model %s has invalid index", model.CanonicalLabel)
		}
		if err := validateRange(index.SourceRange); err != nil {
			return fmt.Errorf("model %s index %s source range: %w", model.CanonicalLabel, index.Name, err)
		}
	}
	for _, constraint := range model.Constraints {
		if constraint.Name == "" || constraint.Type == "" || constraint.SourceRange == nil {
			return fmt.Errorf("model %s has invalid constraint", model.CanonicalLabel)
		}
		if err := validateRange(constraint.SourceRange); err != nil {
			return fmt.Errorf("model %s constraint %s source range: %w", model.CanonicalLabel, constraint.Name, err)
		}
	}
	return nil
}

func validateMethod(method Method) error {
	if method.Name == "" || method.OwnerClass == "" || method.SourceRange == nil {
		return errors.New("invalid metadata")
	}
	if method.AssumedChainable && !method.Chainable {
		return errors.New("assumed chainable method is not chainable")
	}
	if err := validateRange(method.SourceRange); err != nil {
		return fmt.Errorf("source range: %w", err)
	}
	return nil
}

func validateRange(sourceRange *SourceRange) error {
	if sourceRange == nil || sourceRange.FilePath == "" {
		return errors.New("missing range")
	}
	if sourceRange.Start.Line <= 0 || sourceRange.Start.Column < 0 || sourceRange.End.Line <= 0 || sourceRange.End.Column < 0 {
		return errors.New("invalid position")
	}
	if sourceRange.End.Line < sourceRange.Start.Line || (sourceRange.End.Line == sourceRange.Start.Line && sourceRange.End.Column < sourceRange.Start.Column) {
		return errors.New("end precedes start")
	}
	if len(sourceRange.SourceDigest) != 64 || strings.Trim(sourceRange.SourceDigest, "0123456789abcdef") != "" {
		return errors.New("invalid source digest")
	}
	return nil
}

func buildModelIndex(name string, model Model) (*modelIndex, error) {
	index := &modelIndex{
		name:           name,
		model:          model,
		fields:         make(map[string]*FieldRef, len(model.Fields)),
		attnames:       make(map[string]*FieldRef),
		queries:        make(map[string]*FieldRef),
		accessors:      make(map[string]*FieldRef),
		managers:       make(map[string]*ManagerRef, len(model.Managers)),
		querySets:      make(map[string]*QuerySetRef),
		queryByName:    make(map[string]FieldAccess),
		instanceByName: make(map[string]FieldAccess),
	}
	for context := RelationQuery; context < relationContextCount; context++ {
		index.relations[context].byName = make(map[string]FieldAccess)
	}
	fieldNames := sortedFieldNames(model.Fields)
	for _, fieldName := range fieldNames {
		field := model.Fields[fieldName]
		fieldReference := &FieldRef{field: field}
		index.fields[fieldName] = fieldReference
		if existing, exists := index.queries[fieldName]; exists && existing != fieldReference {
			return nil, fmt.Errorf("model %s has duplicate query name %q", model.CanonicalLabel, fieldName)
		}
		index.queries[fieldName] = fieldReference
		if field.Attname != nil && *field.Attname != "" {
			if _, exists := index.attnames[*field.Attname]; exists {
				return nil, fmt.Errorf("model %s has duplicate attname %q", model.CanonicalLabel, *field.Attname)
			}
			index.attnames[*field.Attname] = fieldReference
		}
		if field.RelationDirection != nil && *field.RelationDirection == "reverse" {
			if field.QueryName != nil && *field.QueryName != "" {
				if existing, exists := index.queries[*field.QueryName]; exists && existing != fieldReference {
					return nil, fmt.Errorf("model %s has duplicate query name %q", model.CanonicalLabel, *field.QueryName)
				}
				index.queries[*field.QueryName] = fieldReference
			}
			if field.AccessorName != nil && *field.AccessorName != "" {
				if existing, exists := index.accessors[*field.AccessorName]; exists && existing != fieldReference {
					return nil, fmt.Errorf("model %s has duplicate accessor name %q", model.CanonicalLabel, *field.AccessorName)
				}
				index.accessors[*field.AccessorName] = fieldReference
			}
		}
	}
	for _, method := range model.QuerySetMethods {
		querySet := index.ensureQuerySet(method.OwnerClass)
		methodReference := &MethodRef{method: method}
		querySet.methods[method.Name] = methodReference
		querySet.methodOrder = append(querySet.methodOrder, methodReference)
		index.methodOrder = append(index.methodOrder, methodReference)
	}
	for _, manager := range model.Managers {
		reference := &ManagerRef{
			manager:         manager,
			methods:         make(map[string]*MethodRef, len(manager.Methods)),
			querySetMethods: make(map[string]*MethodRef, len(manager.QuerySetMethods)),
		}
		if manager.QuerySetClass != nil {
			reference.querySet = index.ensureQuerySet(*manager.QuerySetClass)
		}
		for _, method := range manager.Methods {
			methodReference := &MethodRef{method: method}
			reference.methods[method.Name] = methodReference
			reference.methodOrder = append(reference.methodOrder, methodReference)
		}
		for _, binding := range manager.QuerySetMethods {
			methodReference := reference.querySet.methods[binding.Method.Name]
			if methodReference == nil {
				methodReference = &MethodRef{method: binding.Method}
				reference.querySet.methods[binding.Method.Name] = methodReference
				reference.querySet.methodOrder = append(reference.querySet.methodOrder, methodReference)
			}
			if binding.AvailableOnManager {
				reference.querySetMethods[binding.Method.Name] = methodReference
				reference.querySetMethodOrder = append(reference.querySetMethodOrder, methodReference)
			}
		}
		sortMethods(reference.methodOrder)
		sortMethods(reference.querySetMethodOrder)
		index.managers[manager.Name] = reference
		index.managerOrder = append(index.managerOrder, reference)
	}
	for _, fieldName := range fieldNames {
		field := index.fields[fieldName]
		if field.field.RelationDirection != nil && *field.field.RelationDirection == "reverse" {
			queryName := field.field.Name
			if field.field.QueryName != nil && *field.field.QueryName != "" {
				queryName = *field.field.QueryName
			}
			instanceName := field.field.Name
			if field.field.AccessorName != nil && *field.field.AccessorName != "" {
				instanceName = *field.field.AccessorName
			}
			index.addAccess(queryName, FieldAccessReverse, field, true)
			index.addAccess(instanceName, FieldAccessReverse, field, false)
			if field.field.IsRelation {
				if err := index.addRelation(RelationQuery, queryName, FieldAccessReverse, field); err != nil {
					return nil, err
				}
				if field.field.RelationCardinality != nil && *field.field.RelationCardinality == string(RelationOneToOne) && field.field.RelatedModel != nil {
					if err := index.addRelation(RelationSelectRelated, queryName, FieldAccessReverse, field); err != nil {
						return nil, err
					}
				}
				if err := index.addRelation(RelationPrefetchRelated, instanceName, FieldAccessReverse, field); err != nil {
					return nil, err
				}
			}
			continue
		}
		index.addAccess(field.field.Name, FieldAccessDeclared, field, true)
		index.addAccess(field.field.Name, FieldAccessDeclared, field, false)
		if field.field.EffectivePrimaryKey && field.field.Name != "pk" {
			index.addAccess("pk", FieldAccessDeclared, field, true)
			index.addAccess("pk", FieldAccessDeclared, field, false)
		}
		if field.field.IsRelation {
			if err := index.addRelation(RelationQuery, field.field.Name, FieldAccessDeclared, field); err != nil {
				return nil, err
			}
			if field.field.RelationCardinality != nil && field.field.RelatedModel != nil && (*field.field.RelationCardinality == string(RelationManyToOne) || *field.field.RelationCardinality == string(RelationOneToOne)) {
				if err := index.addRelation(RelationSelectRelated, field.field.Name, FieldAccessDeclared, field); err != nil {
					return nil, err
				}
			}
			if err := index.addRelation(RelationPrefetchRelated, field.field.Name, FieldAccessDeclared, field); err != nil {
				return nil, err
			}
		}
		if field.field.Attname != nil && *field.field.Attname != "" && *field.field.Attname != field.field.Name {
			index.addAccess(*field.field.Attname, FieldAccessAttname, field, true)
			index.addAccess(*field.field.Attname, FieldAccessAttname, field, false)
			if field.field.IsRelation {
				if err := index.addRelation(RelationQuery, *field.field.Attname, FieldAccessAttname, field); err != nil {
					return nil, err
				}
			}
		}
	}
	sort.Slice(index.queryAccess, func(left, right int) bool { return accessLess(index.queryAccess[left], index.queryAccess[right]) })
	sort.Slice(index.instanceAccess, func(left, right int) bool { return accessLess(index.instanceAccess[left], index.instanceAccess[right]) })
	for context := RelationQuery; context < relationContextCount; context++ {
		index.relations[context].ordered = make([]FieldAccess, 0, len(index.relations[context].byName))
		for _, access := range index.relations[context].byName {
			index.relations[context].ordered = append(index.relations[context].ordered, access)
		}
		sort.Slice(index.relations[context].ordered, func(left, right int) bool {
			return accessLess(index.relations[context].ordered[left], index.relations[context].ordered[right])
		})
	}
	sort.Slice(index.managerOrder, func(left, right int) bool { return index.managerOrder[left].Name() < index.managerOrder[right].Name() })
	for _, querySet := range index.querySets {
		sortMethods(querySet.methodOrder)
		index.querySetOrder = append(index.querySetOrder, querySet)
	}
	sort.Slice(index.querySetOrder, func(left, right int) bool {
		return index.querySetOrder[left].Class() < index.querySetOrder[right].Class()
	})
	sortMethods(index.methodOrder)
	return index, nil
}

func sortedFieldNames(fields map[string]Field) []string {
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (index *modelIndex) ensureQuerySet(class string) *QuerySetRef {
	if existing := index.querySets[class]; existing != nil {
		return existing
	}
	querySet := &QuerySetRef{class: class, methods: make(map[string]*MethodRef)}
	index.querySets[class] = querySet
	return querySet
}

func sortMethods(methods []*MethodRef) {
	sort.Slice(methods, func(left, right int) bool {
		if methods[left].Name() != methods[right].Name() {
			return methods[left].Name() < methods[right].Name()
		}
		return methods[left].OwnerClass() < methods[right].OwnerClass()
	})
}

func (index *modelIndex) addRelation(context RelationContext, name string, kind FieldAccessKind, field *FieldRef) error {
	if name == "" || name == "+" {
		return nil
	}
	relations := &index.relations[context]
	if existing, exists := relations.byName[name]; exists {
		if existing.Field != field {
			return fmt.Errorf("model %s has duplicate %s relation name %q", index.model.CanonicalLabel, context, name)
		}
		if existing.Kind <= kind {
			return nil
		}
	}
	relations.byName[name] = FieldAccess{Name: name, Kind: kind, Field: field}
	return nil
}

func (context RelationContext) String() string {
	switch context {
	case RelationQuery:
		return "query"
	case RelationSelectRelated:
		return "select_related"
	case RelationPrefetchRelated:
		return "prefetch_related"
	default:
		return "unknown"
	}
}

func (index *modelIndex) addAccess(name string, kind FieldAccessKind, field *FieldRef, query bool) {
	if name == "" || name == "+" {
		return
	}
	access := FieldAccess{Name: name, Kind: kind, Field: field}
	byName := index.instanceByName
	if query {
		byName = index.queryByName
	}
	if existing, exists := byName[name]; exists && existing.Kind <= kind {
		return
	}
	byName[name] = access
	if query {
		index.queryAccess = replaceAccess(index.queryAccess, access)
	} else {
		index.instanceAccess = replaceAccess(index.instanceAccess, access)
	}
}

func replaceAccess(accesses []FieldAccess, replacement FieldAccess) []FieldAccess {
	for index, access := range accesses {
		if access.Name == replacement.Name {
			accesses[index] = replacement
			return accesses
		}
	}
	return append(accesses, replacement)
}

func accessLess(left, right FieldAccess) bool {
	if left.Kind != right.Kind {
		return left.Kind < right.Kind
	}
	return left.Name < right.Name
}

func (graph *Graph) ModelCount() int {
	if graph == nil {
		return 0
	}
	return len(graph.canonical)
}

func (graph *Graph) HasModel(canonicalLabel string) bool {
	if graph == nil {
		return false
	}
	_, exists := graph.canonical[canonicalLabel]
	return exists
}

func (graph *Graph) HasClass(classPath string) bool {
	if graph == nil {
		return false
	}
	_, exists := graph.classPath[classPath]
	return exists
}

func (graph *Graph) CanonicalLabelForClass(classPath string) (string, bool) {
	if graph == nil {
		return "", false
	}
	index := graph.classPath[classPath]
	if index == nil {
		return "", false
	}
	return index.model.CanonicalLabel, true
}

func (graph *Graph) CanonicalLabelForSourceClass(filePath, qualname string) (string, bool) {
	if graph == nil || filePath == "" || qualname == "" {
		return "", false
	}
	index := graph.sourceClass[sourceClassKey(filePath, qualname)]
	if index == nil {
		return "", false
	}
	return index.model.CanonicalLabel, true
}

func (graph *Graph) ModelInfo(canonicalLabel string) (ModelInfo, bool) {
	if graph == nil {
		return ModelInfo{}, false
	}
	index := graph.canonical[canonicalLabel]
	if index == nil {
		return ModelInfo{}, false
	}
	return ModelInfo{
		CanonicalLabel:    index.model.CanonicalLabel,
		FilePath:          index.model.FilePath,
		Managed:           index.model.Managed,
		HasAbstractParent: index.model.HasAbstractParent,
		MultiTableChild:   index.model.MultiTableChild,
		IndexCount:        len(index.model.Indexes),
		ConstraintCount:   len(index.model.Constraints),
	}, true
}

func (graph *Graph) ModelSourceRange(canonicalLabel string) (SourceRange, bool) {
	if graph == nil {
		return SourceRange{}, false
	}
	index := graph.canonical[canonicalLabel]
	if index == nil || index.model.SourceRange == nil {
		return SourceRange{}, false
	}
	return *index.model.SourceRange, true
}

func (graph *Graph) ModuleModelCount(module string) int {
	if graph == nil {
		return 0
	}
	return len(graph.module[module])
}

func (graph *Graph) Field(canonicalLabel, name string) (*FieldRef, bool) {
	if graph == nil {
		return nil, false
	}
	index := graph.canonical[canonicalLabel]
	if index == nil {
		return nil, false
	}
	field, exists := index.fields[name]
	return field, exists
}

func (graph *Graph) RelationFieldForSource(filePath, className, fieldName string, start, end Position) (*FieldRef, bool) {
	if graph == nil || filePath == "" || className == "" || fieldName == "" {
		return nil, false
	}
	filePath = resolvedPath(filePath)
	var selected *FieldRef
	selectedTarget := ""
	for _, index := range graph.canonical {
		for _, field := range index.fields {
			direction, forward := field.RelationDirection()
			sourceRange, hasRange := field.SourceRange()
			if !forward || direction != RelationForward || !hasRange || field.Name() != fieldName || !strings.HasSuffix(field.SourceModel(), "."+className) || sourceRange.Start != start || sourceRange.End != end || !samePath(resolvedPath(sourceRange.FilePath), filePath) {
				continue
			}
			target, ok := field.RuntimeRelatedModel()
			if !ok {
				target, ok = field.RelatedModel()
			}
			if !ok || selected != nil && (selectedTarget != target || !sameSourceRange(selected, field)) {
				return nil, false
			}
			selected = field
			selectedTarget = target
		}
	}
	return selected, selected != nil
}

func sameSourceRange(left, right *FieldRef) bool {
	leftRange, leftOK := left.SourceRange()
	rightRange, rightOK := right.SourceRange()
	return leftOK && rightOK && leftRange == rightRange
}

func (graph *Graph) QueryField(canonicalLabel, name string) (*FieldRef, bool) {
	if graph == nil {
		return nil, false
	}
	index := graph.canonical[canonicalLabel]
	if index == nil {
		return nil, false
	}
	field, exists := index.queries[name]
	return field, exists
}

func (graph *Graph) AccessorField(canonicalLabel, name string) (*FieldRef, bool) {
	if graph == nil {
		return nil, false
	}
	index := graph.canonical[canonicalLabel]
	if index == nil {
		return nil, false
	}
	field, exists := index.accessors[name]
	return field, exists
}

func (graph *Graph) AttnameField(canonicalLabel, name string) (*FieldRef, bool) {
	if graph == nil {
		return nil, false
	}
	index := graph.canonical[canonicalLabel]
	if index == nil {
		return nil, false
	}
	field, exists := index.attnames[name]
	return field, exists
}

func (graph *Graph) Relation(canonicalLabel, name string, context RelationContext) (FieldAccess, bool) {
	if graph == nil || context >= relationContextCount {
		return FieldAccess{}, false
	}
	index := graph.canonical[canonicalLabel]
	if index == nil {
		return FieldAccess{}, false
	}
	access, exists := index.relations[context].byName[name]
	return access, exists
}

func (graph *Graph) QueryRelation(canonicalLabel, name string) (FieldAccess, bool) {
	return graph.Relation(canonicalLabel, name, RelationQuery)
}

func (graph *Graph) SelectRelatedRelation(canonicalLabel, name string) (FieldAccess, bool) {
	return graph.Relation(canonicalLabel, name, RelationSelectRelated)
}

func (graph *Graph) PrefetchRelatedRelation(canonicalLabel, name string) (FieldAccess, bool) {
	return graph.Relation(canonicalLabel, name, RelationPrefetchRelated)
}

func (graph *Graph) Manager(canonicalLabel, name string) (*ManagerRef, bool) {
	if graph == nil {
		return nil, false
	}
	index := graph.canonical[canonicalLabel]
	if index == nil {
		return nil, false
	}
	manager, exists := index.managers[name]
	return manager, exists
}

func (graph *Graph) QuerySet(canonicalLabel, class string) (*QuerySetRef, bool) {
	if graph == nil {
		return nil, false
	}
	index := graph.canonical[canonicalLabel]
	if index == nil {
		return nil, false
	}
	querySet, exists := index.querySets[class]
	return querySet, exists
}

func (graph *Graph) QuerySetMethod(canonicalLabel, class, name string) (*MethodRef, bool) {
	querySet, exists := graph.QuerySet(canonicalLabel, class)
	if !exists {
		return nil, false
	}
	return querySet.Method(name)
}

func (graph *Graph) VisitQueryFields(canonicalLabel string, visit func(FieldAccess) bool) bool {
	return graph.visitFields(canonicalLabel, true, visit)
}

func (graph *Graph) VisitInstanceFields(canonicalLabel string, visit func(FieldAccess) bool) bool {
	return graph.visitFields(canonicalLabel, false, visit)
}

func (graph *Graph) visitFields(canonicalLabel string, query bool, visit func(FieldAccess) bool) bool {
	if graph == nil {
		return false
	}
	index := graph.canonical[canonicalLabel]
	if index == nil {
		return false
	}
	accesses := index.instanceAccess
	if query {
		accesses = index.queryAccess
	}
	for _, access := range accesses {
		if !visit(access) {
			break
		}
	}
	return true
}

func (graph *Graph) VisitManagers(canonicalLabel string, visit func(*ManagerRef) bool) bool {
	if graph == nil {
		return false
	}
	index := graph.canonical[canonicalLabel]
	if index == nil {
		return false
	}
	for _, manager := range index.managerOrder {
		if !visit(manager) {
			break
		}
	}
	return true
}

func (graph *Graph) VisitRelations(canonicalLabel string, context RelationContext, visit func(FieldAccess) bool) bool {
	if graph == nil || context >= relationContextCount {
		return false
	}
	index := graph.canonical[canonicalLabel]
	if index == nil {
		return false
	}
	for _, access := range index.relations[context].ordered {
		if !visit(access) {
			break
		}
	}
	return true
}

func (graph *Graph) VisitQueryRelations(canonicalLabel string, visit func(FieldAccess) bool) bool {
	return graph.VisitRelations(canonicalLabel, RelationQuery, visit)
}

func (graph *Graph) VisitSelectRelatedRelations(canonicalLabel string, visit func(FieldAccess) bool) bool {
	return graph.VisitRelations(canonicalLabel, RelationSelectRelated, visit)
}

func (graph *Graph) VisitPrefetchRelatedRelations(canonicalLabel string, visit func(FieldAccess) bool) bool {
	return graph.VisitRelations(canonicalLabel, RelationPrefetchRelated, visit)
}

func (graph *Graph) VisitQuerySets(canonicalLabel string, visit func(*QuerySetRef) bool) bool {
	if graph == nil {
		return false
	}
	index := graph.canonical[canonicalLabel]
	if index == nil {
		return false
	}
	for _, querySet := range index.querySetOrder {
		if !visit(querySet) {
			break
		}
	}
	return true
}

func (graph *Graph) VisitQuerySetMethods(canonicalLabel string, visit func(*MethodRef) bool) bool {
	if graph == nil {
		return false
	}
	index := graph.canonical[canonicalLabel]
	if index == nil {
		return false
	}
	for _, method := range index.methodOrder {
		if !visit(method) {
			break
		}
	}
	return true
}

func (graph *Graph) QueryAccess(canonicalLabel, name string) (FieldAccess, bool) {
	if graph == nil || graph.canonical[canonicalLabel] == nil {
		return FieldAccess{}, false
	}
	access, exists := graph.canonical[canonicalLabel].queryByName[name]
	return access, exists
}

func (graph *Graph) InstanceAccess(canonicalLabel, name string) (FieldAccess, bool) {
	if graph == nil || graph.canonical[canonicalLabel] == nil {
		return FieldAccess{}, false
	}
	access, exists := graph.canonical[canonicalLabel].instanceByName[name]
	return access, exists
}

func (field *FieldRef) Name() string { return field.field.Name }

func (field *FieldRef) HelpText() string { return field.field.HelpText }

func (field *FieldRef) Type() string { return field.field.Type }

func (field *FieldRef) DBColumn() (string, bool) {
	if field == nil || field.field.DBColumn == nil {
		return "", false
	}
	return *field.field.DBColumn, true
}

func (field *FieldRef) DBType() (string, bool) {
	if field == nil || field.field.DBType == nil {
		return "", false
	}
	return *field.field.DBType, true
}

func (field *FieldRef) IsNullable() bool { return field != nil && field.field.Null }

func (field *FieldRef) IsDBIndexed() bool { return field != nil && field.field.DBIndex }

func (field *FieldRef) IsUnique() bool { return field != nil && field.field.Unique }

func (field *FieldRef) IsPrimaryKey() bool { return field != nil && field.field.PrimaryKey }

func (field *FieldRef) SourceRange() (SourceRange, bool) {
	if field == nil || field.field.SourceRange == nil {
		return SourceRange{}, false
	}
	return *field.field.SourceRange, true
}

func (field *FieldRef) RuntimeRelatedModel() (string, bool) {
	if field == nil || field.field.RuntimeRelatedModel == nil || *field.field.RuntimeRelatedModel == "" {
		return "", false
	}
	return *field.field.RuntimeRelatedModel, true
}

func (field *FieldRef) SourceModel() string {
	if field == nil {
		return ""
	}
	return field.field.SourceModel
}

func (field *FieldRef) IsRelation() bool {
	return field != nil && field.field.IsRelation
}

func (field *FieldRef) RelationDirection() (RelationDirection, bool) {
	if field == nil || field.field.RelationDirection == nil {
		return "", false
	}
	return RelationDirection(*field.field.RelationDirection), true
}

func (field *FieldRef) RelationCardinality() (RelationCardinality, bool) {
	if field == nil || field.field.RelationCardinality == nil {
		return "", false
	}
	return RelationCardinality(*field.field.RelationCardinality), true
}

func (field *FieldRef) Attname() (string, bool) {
	if field == nil || field.field.Attname == nil {
		return "", false
	}
	return *field.field.Attname, true
}

func (field *FieldRef) QueryName() (string, bool) {
	if field == nil || field.field.QueryName == nil {
		return "", false
	}
	return *field.field.QueryName, true
}

func (field *FieldRef) AccessorName() (string, bool) {
	if field == nil || field.field.AccessorName == nil {
		return "", false
	}
	return *field.field.AccessorName, true
}

func (field *FieldRef) Lookups() []string {
	if field == nil {
		return nil
	}
	return cloneStrings(field.field.Lookups)
}

func (field *FieldRef) UnsupportedLookups() []string {
	if field == nil {
		return nil
	}
	return cloneStrings(field.field.UnsupportedLookups)
}

func (field *FieldRef) Transforms() []string {
	if field == nil {
		return nil
	}
	return cloneStrings(field.field.Transforms)
}

func (field *FieldRef) LookupPaths() []LookupPath {
	if field == nil {
		return nil
	}
	paths := make([]LookupPath, len(field.field.LookupPaths))
	for index, path := range field.field.LookupPaths {
		paths[index] = cloneLookupPath(path)
	}
	return paths
}

func (field *FieldRef) VisitLookupPaths(visit func(LookupPath) bool) bool {
	if field == nil {
		return false
	}
	for _, path := range field.field.LookupPaths {
		if !visit(cloneLookupPath(path)) {
			break
		}
	}
	return true
}

func (field *FieldRef) LookupPathsTruncated() bool {
	return field != nil && field.field.LookupPathsTruncated
}

func (field *FieldRef) RelatedModel() (string, bool) {
	if field == nil || field.related == nil {
		return "", false
	}
	return field.related.model.CanonicalLabel, true
}

func (manager *ManagerRef) Name() string {
	if manager == nil {
		return ""
	}
	return manager.manager.Name
}

func (manager *ManagerRef) OwnerClass() string {
	if manager == nil {
		return ""
	}
	return manager.manager.OwnerClass
}

func (manager *ManagerRef) SourceRange() (SourceRange, bool) {
	if manager == nil || manager.manager.SourceRange == nil {
		return SourceRange{}, false
	}
	return *manager.manager.SourceRange, true
}

func (manager *ManagerRef) QuerySetClass() (string, bool) {
	if manager == nil || manager.manager.QuerySetClass == nil {
		return "", false
	}
	return *manager.manager.QuerySetClass, true
}

func (manager *ManagerRef) QuerySet() (*QuerySetRef, bool) {
	if manager == nil || manager.querySet == nil {
		return nil, false
	}
	return manager.querySet, true
}

func (manager *ManagerRef) Method(name string) (*MethodRef, bool) {
	if manager == nil {
		return nil, false
	}
	method, exists := manager.methods[name]
	return method, exists
}

func (manager *ManagerRef) QuerySetMethod(name string) (*MethodRef, bool) {
	if manager == nil {
		return nil, false
	}
	method, exists := manager.querySetMethods[name]
	return method, exists
}

func (manager *ManagerRef) VisitMethods(visit func(*MethodRef) bool) bool {
	if manager == nil {
		return false
	}
	for _, method := range manager.methodOrder {
		if !visit(method) {
			break
		}
	}
	return true
}

func (manager *ManagerRef) VisitQuerySetMethods(visit func(*MethodRef) bool) bool {
	if manager == nil {
		return false
	}
	for _, method := range manager.querySetMethodOrder {
		if !visit(method) {
			break
		}
	}
	return true
}

func (manager *ManagerRef) IsDefault() bool {
	return manager != nil && manager.manager.Default
}

func (manager *ManagerRef) IsLocal() bool {
	return manager != nil && manager.manager.Local
}

func (manager *ManagerRef) IsAutoCreated() bool {
	return manager != nil && manager.manager.AutoCreated
}

func (querySet *QuerySetRef) Class() string {
	if querySet == nil {
		return ""
	}
	return querySet.class
}

func (querySet *QuerySetRef) Method(name string) (*MethodRef, bool) {
	if querySet == nil {
		return nil, false
	}
	method, exists := querySet.methods[name]
	return method, exists
}

func (querySet *QuerySetRef) VisitMethods(visit func(*MethodRef) bool) bool {
	if querySet == nil {
		return false
	}
	for _, method := range querySet.methodOrder {
		if !visit(method) {
			break
		}
	}
	return true
}

func (method *MethodRef) Name() string {
	if method == nil {
		return ""
	}
	return method.method.Name
}

func (method *MethodRef) OwnerClass() string {
	if method == nil {
		return ""
	}
	return method.method.OwnerClass
}

func (method *MethodRef) Signature() (string, bool) {
	if method == nil || method.method.Signature == nil {
		return "", false
	}
	return *method.method.Signature, true
}

func (method *MethodRef) Docstring() (string, bool) {
	if method == nil || method.method.Docstring == nil {
		return "", false
	}
	return *method.method.Docstring, true
}

func (method *MethodRef) SourceRange() (SourceRange, bool) {
	if method == nil || method.method.SourceRange == nil {
		return SourceRange{}, false
	}
	return *method.method.SourceRange, true
}

func (method *MethodRef) Chainable() bool {
	return method != nil && method.method.Chainable
}

func (method *MethodRef) AssumedChainable() bool {
	return method != nil && method.method.AssumedChainable
}

func cloneStrings(values []string) []string {
	if values == nil {
		return nil
	}
	return append([]string{}, values...)
}

func cloneLookupPath(path LookupPath) LookupPath {
	return LookupPath{
		Transforms: cloneStrings(path.Transforms),
		Kinds:      cloneStrings(path.Kinds),
		Lookups:    cloneStrings(path.Lookups),
	}
}

func cloneSnapshot(snapshot Snapshot) (Snapshot, error) {
	clone := snapshot
	clone.SchemaSources = cloneStrings(snapshot.SchemaSources)
	clone.Apps = make(map[string]App, len(snapshot.Apps))
	for name, app := range snapshot.Apps {
		clone.Apps[name] = cloneApp(app)
	}
	return clone, nil
}

func cloneApp(app App) App {
	clone := app
	clone.Models = make(map[string]Model, len(app.Models))
	for name, model := range app.Models {
		clone.Models[name] = cloneModel(model)
	}
	return clone
}

func cloneModel(model Model) Model {
	clone := model
	clone.SourceRange = clonePointer(model.SourceRange)
	clone.Docstring = clonePointer(model.Docstring)
	clone.Parents = cloneSlice(model.Parents)
	for index := range clone.Parents {
		clone.Parents[index].ParentLink = clonePointer(model.Parents[index].ParentLink)
		clone.Parents[index].SourceRange = clonePointer(model.Parents[index].SourceRange)
	}
	clone.CustomManagers = cloneStrings(model.CustomManagers)
	clone.Managers = cloneSlice(model.Managers)
	for index := range clone.Managers {
		clone.Managers[index] = cloneManager(model.Managers[index])
	}
	clone.QuerySetMethods = cloneMethods(model.QuerySetMethods)
	clone.Indexes = cloneSlice(model.Indexes)
	for index := range clone.Indexes {
		clone.Indexes[index] = cloneIndex(model.Indexes[index])
	}
	clone.Constraints = cloneSlice(model.Constraints)
	for index := range clone.Constraints {
		clone.Constraints[index] = cloneConstraint(model.Constraints[index])
	}
	clone.Fields = make(map[string]Field, len(model.Fields))
	for name, field := range model.Fields {
		clone.Fields[name] = cloneField(field)
	}
	return clone
}

func cloneManager(manager Manager) Manager {
	clone := manager
	clone.QuerySetClass = clonePointer(manager.QuerySetClass)
	clone.SourceRange = clonePointer(manager.SourceRange)
	clone.Methods = cloneMethods(manager.Methods)
	clone.QuerySetMethods = cloneSlice(manager.QuerySetMethods)
	for index := range clone.QuerySetMethods {
		clone.QuerySetMethods[index].Method = cloneMethod(manager.QuerySetMethods[index].Method)
	}
	return clone
}

func cloneMethods(methods []Method) []Method {
	clone := cloneSlice(methods)
	for index := range clone {
		clone[index] = cloneMethod(methods[index])
	}
	return clone
}

func cloneMethod(method Method) Method {
	clone := method
	clone.Signature = clonePointer(method.Signature)
	clone.Docstring = clonePointer(method.Docstring)
	clone.SourceRange = clonePointer(method.SourceRange)
	return clone
}

func cloneField(field Field) Field {
	clone := field
	clone.RelatedModel = clonePointer(field.RelatedModel)
	clone.RuntimeRelatedModel = clonePointer(field.RuntimeRelatedModel)
	clone.Lookups = cloneStrings(field.Lookups)
	clone.UnsupportedLookups = cloneStrings(field.UnsupportedLookups)
	clone.Attname = clonePointer(field.Attname)
	clone.DBColumn = clonePointer(field.DBColumn)
	clone.DBType = clonePointer(field.DBType)
	clone.RelationCardinality = clonePointer(field.RelationCardinality)
	clone.RelationDirection = clonePointer(field.RelationDirection)
	clone.AccessorName = clonePointer(field.AccessorName)
	clone.QueryName = clonePointer(field.QueryName)
	clone.SourceRange = clonePointer(field.SourceRange)
	clone.Transforms = cloneStrings(field.Transforms)
	clone.LookupPaths = cloneSlice(field.LookupPaths)
	for index := range clone.LookupPaths {
		clone.LookupPaths[index] = cloneLookupPath(field.LookupPaths[index])
	}
	return clone
}

func cloneIndex(index Index) Index {
	clone := index
	clone.Fields = cloneSlice(index.Fields)
	clone.Expressions = cloneStrings(index.Expressions)
	clone.Condition = clonePointer(index.Condition)
	clone.Include = cloneStrings(index.Include)
	clone.Opclasses = cloneStrings(index.Opclasses)
	clone.DBTablespace = clonePointer(index.DBTablespace)
	clone.SourceRange = clonePointer(index.SourceRange)
	return clone
}

func cloneConstraint(constraint Constraint) Constraint {
	clone := constraint
	clone.Fields = cloneStrings(constraint.Fields)
	clone.Expressions = cloneStrings(constraint.Expressions)
	clone.Condition = clonePointer(constraint.Condition)
	clone.Include = cloneStrings(constraint.Include)
	clone.Opclasses = cloneStrings(constraint.Opclasses)
	clone.Deferrable = clonePointer(constraint.Deferrable)
	clone.NullsDistinct = clonePointer(constraint.NullsDistinct)
	clone.ViolationErrorCode = clonePointer(constraint.ViolationErrorCode)
	clone.ViolationErrorMessage = clonePointer(constraint.ViolationErrorMessage)
	clone.SourceRange = clonePointer(constraint.SourceRange)
	return clone
}

func cloneSlice[T any](values []T) []T {
	if values == nil {
		return nil
	}
	clone := make([]T, len(values))
	copy(clone, values)
	return clone
}

func clonePointer[T any](value *T) *T {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

func ModelName(canonicalLabel string) string {
	_, name, _ := strings.Cut(canonicalLabel, ".")
	return name
}
