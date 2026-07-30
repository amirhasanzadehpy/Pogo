package schema

type Snapshot struct {
	SchemaVersion           int            `json:"schema_version"`
	PositionEncoding        string         `json:"position_encoding"`
	LookupTransformMaxDepth int            `json:"lookup_transform_max_depth"`
	LookupPathMaxCount      int            `json:"lookup_path_max_count"`
	SchemaSources           []string       `json:"schema_sources"`
	Apps                    map[string]App `json:"apps"`
}

type App struct {
	Label      string           `json:"label"`
	ImportName string           `json:"import_name"`
	RootPath   string           `json:"root_path"`
	Models     map[string]Model `json:"models"`
}

type Model struct {
	CanonicalLabel    string           `json:"canonical_label"`
	Module            string           `json:"module"`
	Qualname          string           `json:"qualname"`
	FilePath          string           `json:"file_path"`
	LineNumber        int              `json:"line_number"`
	SourceRange       *SourceRange     `json:"source_range"`
	Docstring         *string          `json:"docstring"`
	Abstract          bool             `json:"abstract"`
	Proxy             bool             `json:"proxy"`
	Managed           bool             `json:"managed"`
	Swapped           bool             `json:"swapped"`
	HasAbstractParent bool             `json:"has_abstract_parent"`
	MultiTableChild   bool             `json:"multi_table_child"`
	Parents           []Parent         `json:"parents"`
	DefaultManager    string           `json:"default_manager"`
	BaseManager       BaseManager      `json:"base_manager"`
	CustomManagers    []string         `json:"custom_managers"`
	Managers          []Manager        `json:"managers"`
	QuerySetMethods   []Method         `json:"queryset_methods"`
	Indexes           []Index          `json:"indexes"`
	Constraints       []Constraint     `json:"constraints"`
	Fields            map[string]Field `json:"fields"`
}

type Parent struct {
	CanonicalLabel string       `json:"canonical_label"`
	ClassPath      string       `json:"class_path"`
	Abstract       bool         `json:"abstract"`
	Proxy          bool         `json:"proxy"`
	ParentLink     *string      `json:"parent_link"`
	SourceRange    *SourceRange `json:"source_range"`
}

type BaseManager struct {
	Name       string `json:"name"`
	OwnerClass string `json:"owner_class"`
}

type Manager struct {
	Name            string                `json:"name"`
	OwnerClass      string                `json:"owner_class"`
	QuerySetClass   *string               `json:"queryset_class"`
	Default         bool                  `json:"default"`
	Local           bool                  `json:"local"`
	AutoCreated     bool                  `json:"auto_created"`
	SourceRange     *SourceRange          `json:"source_range"`
	Methods         []Method              `json:"methods"`
	QuerySetMethods []BoundQuerySetMethod `json:"queryset_methods"`
}

type BoundQuerySetMethod struct {
	Method             Method `json:"method"`
	AvailableOnManager bool   `json:"available_on_manager"`
}

type Method struct {
	Name             string       `json:"name"`
	OwnerClass       string       `json:"owner_class"`
	Signature        *string      `json:"signature"`
	Docstring        *string      `json:"docstring"`
	SourceRange      *SourceRange `json:"source_range"`
	Chainable        bool         `json:"chainable"`
	AssumedChainable bool         `json:"assumed_chainable"`
}

type Field struct {
	Type                 string       `json:"type"`
	IsRelation           bool         `json:"is_relation"`
	RelatedModel         *string      `json:"related_model"`
	RuntimeRelatedModel  *string      `json:"runtime_related_model"`
	Lookups              []string     `json:"lookups"`
	UnsupportedLookups   []string     `json:"unsupported_lookups"`
	HelpText             string       `json:"help_text"`
	Name                 string       `json:"name"`
	Attname              *string      `json:"attname"`
	DBColumn             *string      `json:"db_column"`
	DBType               *string      `json:"db_type"`
	InternalType         string       `json:"internal_type"`
	Null                 bool         `json:"null"`
	DBIndex              bool         `json:"db_index"`
	Unique               bool         `json:"unique"`
	PrimaryKey           bool         `json:"primary_key"`
	EffectivePrimaryKey  bool         `json:"effective_primary_key"`
	Concrete             bool         `json:"concrete"`
	AutoCreated          bool         `json:"auto_created"`
	RelationCardinality  *string      `json:"relation_cardinality"`
	RelationDirection    *string      `json:"relation_direction"`
	AccessorName         *string      `json:"accessor_name"`
	QueryName            *string      `json:"query_name"`
	SourceModel          string       `json:"source_model"`
	SourceModelAbstract  bool         `json:"source_model_abstract"`
	SourceRange          *SourceRange `json:"source_range"`
	ParentLink           bool         `json:"parent_link"`
	Transforms           []string     `json:"transforms"`
	LookupPaths          []LookupPath `json:"lookup_paths"`
	LookupPathsTruncated bool         `json:"lookup_paths_truncated"`
}

type Index struct {
	Name         string       `json:"name"`
	Fields       []IndexField `json:"fields"`
	Expressions  []string     `json:"expressions"`
	Condition    *string      `json:"condition"`
	Include      []string     `json:"include"`
	Opclasses    []string     `json:"opclasses"`
	DBTablespace *string      `json:"db_tablespace"`
	SourceRange  *SourceRange `json:"source_range"`
}

type IndexField struct {
	Name  string `json:"name"`
	Order string `json:"order"`
}

type Constraint struct {
	Name                  string       `json:"name"`
	Type                  string       `json:"type"`
	Fields                []string     `json:"fields"`
	Expressions           []string     `json:"expressions"`
	Condition             *string      `json:"condition"`
	Include               []string     `json:"include"`
	Opclasses             []string     `json:"opclasses"`
	Deferrable            *string      `json:"deferrable"`
	NullsDistinct         *bool        `json:"nulls_distinct"`
	ViolationErrorCode    *string      `json:"violation_error_code"`
	ViolationErrorMessage *string      `json:"violation_error_message"`
	SourceRange           *SourceRange `json:"source_range"`
}

type LookupPath struct {
	Transforms []string `json:"transforms"`
	Kinds      []string `json:"kinds"`
	Lookups    []string `json:"lookups"`
}

type SourceRange struct {
	FilePath     string   `json:"file_path"`
	SourceDigest string   `json:"source_digest,omitempty"`
	Start        Position `json:"start"`
	End          Position `json:"end"`
}

type Position struct {
	Line   int `json:"line"`
	Column int `json:"column"`
}
