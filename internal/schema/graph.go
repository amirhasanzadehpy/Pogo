package schema

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

const (
	Version          = 1
	PositionEncoding = "utf-8-bytes"
	maxLookupDepth   = 2
	maxLookupPaths   = 512
)

type Graph struct {
	snapshot  Snapshot
	canonical map[string]*modelIndex
	classPath map[string]*modelIndex
	module    map[string][]*modelIndex
}

type modelIndex struct {
	name           string
	model          Model
	fields         map[string]*FieldRef
	attnames       map[string]*FieldRef
	queries        map[string]*FieldRef
	accessors      map[string]*FieldRef
	managers       map[string]*ManagerRef
	queryAccess    []FieldAccess
	instanceAccess []FieldAccess
	managerOrder   []*ManagerRef
	queryByName    map[string]FieldAccess
	instanceByName map[string]FieldAccess
}

type FieldRef struct {
	field   Field
	related *modelIndex
}

type ManagerRef struct {
	manager Manager
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
		snapshot:  snapshot,
		canonical: make(map[string]*modelIndex),
		classPath: make(map[string]*modelIndex),
		module:    make(map[string][]*modelIndex),
	}
	for appKey, app := range snapshot.Apps {
		if appKey == "" || app.Label != appKey || app.ImportName == "" || app.RootPath == "" {
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
			graph.module[model.Module] = append(graph.module[model.Module], index)
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
		for _, method := range manager.Methods {
			if method.Name == "" || method.OwnerClass == "" || method.SourceRange == nil {
				return fmt.Errorf("model %s manager %s has invalid method", model.CanonicalLabel, manager.Name)
			}
			if err := validateRange(method.SourceRange); err != nil {
				return fmt.Errorf("model %s manager %s method %s source range: %w", model.CanonicalLabel, manager.Name, method.Name, err)
			}
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
	for _, method := range model.QuerySetMethods {
		if method.Name == "" || method.OwnerClass == "" || method.SourceRange == nil {
			return fmt.Errorf("model %s has invalid QuerySet method", model.CanonicalLabel)
		}
		if err := validateRange(method.SourceRange); err != nil {
			return fmt.Errorf("model %s QuerySet method %s source range: %w", model.CanonicalLabel, method.Name, err)
		}
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
		queryByName:    make(map[string]FieldAccess),
		instanceByName: make(map[string]FieldAccess),
	}
	for fieldName, field := range model.Fields {
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
	for _, manager := range model.Managers {
		reference := &ManagerRef{manager: manager}
		index.managers[manager.Name] = reference
		index.managerOrder = append(index.managerOrder, reference)
	}
	for _, field := range index.fields {
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
			continue
		}
		index.addAccess(field.field.Name, FieldAccessDeclared, field, true)
		index.addAccess(field.field.Name, FieldAccessDeclared, field, false)
		if field.field.Attname != nil && *field.field.Attname != "" && *field.field.Attname != field.field.Name {
			index.addAccess(*field.field.Attname, FieldAccessAttname, field, true)
			index.addAccess(*field.field.Attname, FieldAccessAttname, field, false)
		}
	}
	sort.Slice(index.queryAccess, func(left, right int) bool { return accessLess(index.queryAccess[left], index.queryAccess[right]) })
	sort.Slice(index.instanceAccess, func(left, right int) bool { return accessLess(index.instanceAccess[left], index.instanceAccess[right]) })
	sort.Slice(index.managerOrder, func(left, right int) bool { return index.managerOrder[left].Name() < index.managerOrder[right].Name() })
	return index, nil
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

func (field *FieldRef) SourceModel() string {
	if field == nil {
		return ""
	}
	return field.field.SourceModel
}

func (field *FieldRef) RelatedModel() (string, bool) {
	if field == nil || field.related == nil {
		return "", false
	}
	return field.related.model.CanonicalLabel, true
}

func (manager *ManagerRef) Name() string { return manager.manager.Name }

func (manager *ManagerRef) OwnerClass() string { return manager.manager.OwnerClass }

func cloneSnapshot(snapshot Snapshot) (Snapshot, error) {
	payload, err := json.Marshal(snapshot)
	if err != nil {
		return Snapshot{}, fmt.Errorf("clone snapshot: %w", err)
	}
	var clone Snapshot
	if err := json.Unmarshal(payload, &clone); err != nil {
		return Snapshot{}, fmt.Errorf("clone snapshot: %w", err)
	}
	return clone, nil
}

func ModelName(canonicalLabel string) string {
	_, name, _ := strings.Cut(canonicalLabel, ".")
	return name
}
