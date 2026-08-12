package schema

import (
	"encoding/json"
	"fmt"
)

func ValidateWire(payload []byte) error {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(payload, &root); err != nil {
		return err
	}
	if err := requireKeys(root, "schema_version", "position_encoding", "lookup_transform_max_depth", "lookup_path_max_count", "schema_sources", "schema_sources_complete", "apps"); err != nil {
		return fmt.Errorf("snapshot: %w", err)
	}
	if err := requireKinds(root, map[string]jsonKind{
		"schema_version": kindNumber, "position_encoding": kindString, "lookup_transform_max_depth": kindNumber,
		"lookup_path_max_count": kindNumber, "schema_sources": kindArray, "schema_sources_complete": kindBool, "apps": kindObject,
	}); err != nil {
		return fmt.Errorf("snapshot: %w", err)
	}
	if err := validateStringArray(root["schema_sources"]); err != nil {
		return fmt.Errorf("schema_sources: %w", err)
	}
	var apps map[string]json.RawMessage
	if err := json.Unmarshal(root["apps"], &apps); err != nil {
		return fmt.Errorf("snapshot apps: %w", err)
	}
	for appName, payload := range apps {
		if err := validateAppWire(payload); err != nil {
			return fmt.Errorf("app %s: %w", appName, err)
		}
	}
	return nil
}

func validateAppWire(payload []byte) error {
	object, err := rawObject(payload)
	if err != nil {
		return err
	}
	if err := requireKeys(object, "label", "import_name", "root_path", "models"); err != nil {
		return err
	}
	if err := requireKinds(object, map[string]jsonKind{"label": kindString, "import_name": kindString, "root_path": kindString, "models": kindObject}); err != nil {
		return err
	}
	var models map[string]json.RawMessage
	if err := json.Unmarshal(object["models"], &models); err != nil {
		return err
	}
	for name, model := range models {
		if err := validateModelWire(model); err != nil {
			return fmt.Errorf("model %s: %w", name, err)
		}
	}
	return nil
}

func validateModelWire(payload []byte) error {
	object, err := rawObject(payload)
	if err != nil {
		return err
	}
	if err := requireKeys(object,
		"canonical_label", "module", "qualname", "file_path", "line_number", "source_range", "docstring",
		"abstract", "proxy", "managed", "swapped", "has_abstract_parent", "multi_table_child", "parents",
		"default_manager", "base_manager", "custom_managers", "managers", "queryset_methods", "indexes", "constraints", "fields"); err != nil {
		return err
	}
	if err := requireKinds(object, map[string]jsonKind{
		"canonical_label": kindString, "module": kindString, "qualname": kindString, "file_path": kindString,
		"line_number": kindNumber, "source_range": kindObject, "docstring": kindNullableString, "abstract": kindBool,
		"proxy": kindBool, "managed": kindBool, "swapped": kindBool, "has_abstract_parent": kindBool,
		"multi_table_child": kindBool, "parents": kindArray, "default_manager": kindString, "base_manager": kindObject,
		"custom_managers": kindArray, "managers": kindArray, "queryset_methods": kindArray, "indexes": kindArray,
		"constraints": kindArray, "fields": kindObject,
	}); err != nil {
		return err
	}
	if err := validateSourceRangeWire(object["source_range"]); err != nil {
		return fmt.Errorf("source_range: %w", err)
	}
	if err := validateStringArray(object["custom_managers"]); err != nil {
		return fmt.Errorf("custom_managers: %w", err)
	}
	if err := validateTypedObject(object["base_manager"], map[string]jsonKind{"name": kindString, "owner_class": kindString}); err != nil {
		return fmt.Errorf("base_manager: %w", err)
	}
	if err := validateArray(object["parents"], func(value json.RawMessage) error {
		parent, err := rawObject(value)
		if err != nil {
			return err
		}
		if err := requireKinds(parent, map[string]jsonKind{"canonical_label": kindString, "class_path": kindString, "abstract": kindBool, "proxy": kindBool, "parent_link": kindNullableString, "source_range": kindObject}); err != nil {
			return err
		}
		return validateSourceRangeWire(parent["source_range"])
	}); err != nil {
		return fmt.Errorf("parents: %w", err)
	}
	if err := validateArray(object["managers"], validateManagerWire); err != nil {
		return fmt.Errorf("managers: %w", err)
	}
	if err := validateArray(object["queryset_methods"], validateMethodWire); err != nil {
		return fmt.Errorf("queryset_methods: %w", err)
	}
	if err := validateArray(object["indexes"], func(value json.RawMessage) error {
		index, err := rawObject(value)
		if err != nil {
			return err
		}
		if err := requireKinds(index, map[string]jsonKind{"name": kindString, "fields": kindArray, "expressions": kindArray, "condition": kindNullableString, "include": kindArray, "opclasses": kindArray, "db_tablespace": kindNullableString, "source_range": kindObject}); err != nil {
			return err
		}
		if err := validateArray(index["fields"], func(field json.RawMessage) error {
			return validateTypedObject(field, map[string]jsonKind{"name": kindString, "order": kindString})
		}); err != nil {
			return err
		}
		for _, key := range []string{"expressions", "include", "opclasses"} {
			if err := validateStringArray(index[key]); err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
		}
		return validateSourceRangeWire(index["source_range"])
	}); err != nil {
		return fmt.Errorf("indexes: %w", err)
	}
	if err := validateArray(object["constraints"], func(value json.RawMessage) error {
		constraint, err := rawObject(value)
		if err != nil {
			return err
		}
		if err := requireKinds(constraint, map[string]jsonKind{
			"name": kindString, "type": kindString, "fields": kindArray, "expressions": kindArray,
			"condition": kindNullableString, "include": kindArray, "opclasses": kindArray, "deferrable": kindNullableString,
			"nulls_distinct": kindNullableBool, "violation_error_code": kindNullableString,
			"violation_error_message": kindNullableString, "source_range": kindObject,
		}); err != nil {
			return err
		}
		for _, key := range []string{"fields", "expressions", "include", "opclasses"} {
			if err := validateStringArray(constraint[key]); err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
		}
		return validateSourceRangeWire(constraint["source_range"])
	}); err != nil {
		return fmt.Errorf("constraints: %w", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(object["fields"], &fields); err != nil {
		return err
	}
	for name, field := range fields {
		if err := validateFieldWire(field); err != nil {
			return fmt.Errorf("field %s: %w", name, err)
		}
	}
	return nil
}

func validateManagerWire(payload json.RawMessage) error {
	object, err := rawObject(payload)
	if err != nil {
		return err
	}
	if err := requireKeys(object, "name", "owner_class", "queryset_class", "default", "local", "auto_created", "source_range", "methods", "queryset_methods"); err != nil {
		return err
	}
	if err := requireKinds(object, map[string]jsonKind{"name": kindString, "owner_class": kindString, "queryset_class": kindNullableString, "default": kindBool, "local": kindBool, "auto_created": kindBool, "source_range": kindObject, "methods": kindArray, "queryset_methods": kindArray}); err != nil {
		return err
	}
	if err := validateSourceRangeWire(object["source_range"]); err != nil {
		return err
	}
	if err := validateArray(object["methods"], validateMethodWire); err != nil {
		return err
	}
	return validateArray(object["queryset_methods"], func(value json.RawMessage) error {
		binding, err := rawObject(value)
		if err != nil {
			return err
		}
		if err := requireKeys(binding, "method", "available_on_manager"); err != nil {
			return err
		}
		if err := requireKinds(binding, map[string]jsonKind{"method": kindObject, "available_on_manager": kindBool}); err != nil {
			return err
		}
		return validateMethodWire(binding["method"])
	})
}

func validateMethodWire(payload json.RawMessage) error {
	object, err := rawObject(payload)
	if err != nil {
		return err
	}
	if err := requireKinds(object, map[string]jsonKind{"name": kindString, "owner_class": kindString, "signature": kindNullableString, "docstring": kindNullableString, "source_range": kindObject, "chainable": kindBool, "assumed_chainable": kindBool}); err != nil {
		return err
	}
	return validateSourceRangeWire(object["source_range"])
}

func validateFieldWire(payload json.RawMessage) error {
	object, err := rawObject(payload)
	if err != nil {
		return err
	}
	if err := requireKeys(object,
		"type", "is_relation", "related_model", "runtime_related_model", "lookups", "unsupported_lookups", "help_text",
		"name", "attname", "db_column", "db_type", "internal_type", "null", "db_index", "unique", "primary_key",
		"effective_primary_key", "concrete", "auto_created", "relation_cardinality", "relation_direction", "accessor_name",
		"query_name", "source_model", "source_model_abstract", "source_range", "parent_link", "transforms", "lookup_paths", "lookup_paths_truncated"); err != nil {
		return err
	}
	if err := requireKinds(object, map[string]jsonKind{
		"type": kindString, "is_relation": kindBool, "related_model": kindNullableString,
		"runtime_related_model": kindNullableString, "lookups": kindArray, "unsupported_lookups": kindArray,
		"help_text": kindString, "name": kindString, "attname": kindNullableString, "db_column": kindNullableString,
		"db_type": kindNullableString, "internal_type": kindString, "null": kindBool, "db_index": kindBool,
		"unique": kindBool, "primary_key": kindBool, "effective_primary_key": kindBool, "concrete": kindBool,
		"auto_created": kindBool, "relation_cardinality": kindNullableString, "relation_direction": kindNullableString,
		"accessor_name": kindNullableString, "query_name": kindNullableString, "source_model": kindString,
		"source_model_abstract": kindBool, "source_range": kindObject, "parent_link": kindBool,
		"transforms": kindArray, "lookup_paths": kindArray, "lookup_paths_truncated": kindBool,
	}); err != nil {
		return err
	}
	if err := validateSourceRangeWire(object["source_range"]); err != nil {
		return err
	}
	for _, key := range []string{"lookups", "unsupported_lookups", "transforms"} {
		if err := validateStringArray(object[key]); err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
	}
	return validateArray(object["lookup_paths"], func(value json.RawMessage) error {
		path, err := rawObject(value)
		if err != nil {
			return err
		}
		if err := requireKinds(path, map[string]jsonKind{"transforms": kindArray, "kinds": kindArray, "lookups": kindArray}); err != nil {
			return err
		}
		for _, key := range []string{"transforms", "kinds", "lookups"} {
			if err := validateStringArray(path[key]); err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
		}
		return nil
	})
}

func validateSourceRangeWire(payload json.RawMessage) error {
	object, err := rawObject(payload)
	if err != nil {
		return err
	}
	if err := requireKinds(object, map[string]jsonKind{"file_path": kindString, "source_digest": kindString, "start": kindObject, "end": kindObject}); err != nil {
		return err
	}
	for _, key := range []string{"start", "end"} {
		if err := validateTypedObject(object[key], map[string]jsonKind{"line": kindNumber, "column": kindNumber}); err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
	}
	return nil
}

func validateSimpleObject(payload json.RawMessage, keys ...string) error {
	object, err := rawObject(payload)
	if err != nil {
		return err
	}
	return requireKeys(object, keys...)
}

func validateTypedObject(payload json.RawMessage, kinds map[string]jsonKind) error {
	object, err := rawObject(payload)
	if err != nil {
		return err
	}
	return requireKinds(object, kinds)
}

func rawObject(payload []byte) (map[string]json.RawMessage, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(payload, &object); err != nil {
		return nil, err
	}
	if object == nil {
		return nil, fmt.Errorf("expected object")
	}
	return object, nil
}

func validateArray(payload json.RawMessage, validate func(json.RawMessage) error) error {
	var values []json.RawMessage
	if err := json.Unmarshal(payload, &values); err != nil {
		return err
	}
	for index, value := range values {
		if err := validate(value); err != nil {
			return fmt.Errorf("item %d: %w", index, err)
		}
	}
	return nil
}

func validateStringArray(payload json.RawMessage) error {
	var values []json.RawMessage
	if err := json.Unmarshal(payload, &values); err != nil {
		return err
	}
	if values == nil && detectJSONKind(payload) != kindArray {
		return fmt.Errorf("expected array")
	}
	for index, value := range values {
		if detectJSONKind(value) != kindString {
			return fmt.Errorf("item %d must be string", index)
		}
	}
	return nil
}

func requireKeys(object map[string]json.RawMessage, keys ...string) error {
	for _, key := range keys {
		if _, exists := object[key]; !exists {
			return fmt.Errorf("missing required key %q", key)
		}
	}
	return nil
}

type jsonKind string

const (
	kindArray          jsonKind = "array"
	kindBool           jsonKind = "boolean"
	kindNullableBool   jsonKind = "boolean or null"
	kindNumber         jsonKind = "number"
	kindObject         jsonKind = "object"
	kindString         jsonKind = "string"
	kindNullableString jsonKind = "string or null"
)

func requireKinds(object map[string]json.RawMessage, kinds map[string]jsonKind) error {
	keys := make([]string, 0, len(kinds))
	for key := range kinds {
		keys = append(keys, key)
	}
	if err := requireKeys(object, keys...); err != nil {
		return err
	}
	for key, expected := range kinds {
		actual := detectJSONKind(object[key])
		valid := actual == expected
		if expected == kindNullableString {
			valid = actual == kindString || actual == "null"
		}
		if expected == kindNullableBool {
			valid = actual == kindBool || actual == "null"
		}
		if !valid {
			return fmt.Errorf("key %q must be %s, got %s", key, expected, actual)
		}
	}
	return nil
}

func detectJSONKind(payload json.RawMessage) jsonKind {
	for _, value := range payload {
		switch value {
		case ' ', '\t', '\r', '\n':
			continue
		case '{':
			return kindObject
		case '[':
			return kindArray
		case '"':
			return kindString
		case 't', 'f':
			return kindBool
		case 'n':
			return "null"
		default:
			return kindNumber
		}
	}
	return "missing"
}
