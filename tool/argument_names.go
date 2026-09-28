package tool

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
)

// encoding/json accepts case-insensitive field names. An open schema instead
// treats such a key as an unrelated extra, so it could validate one value and
// pass a different value to the callback. Refuse aliases while leaving truly
// unknown keys, and the original Call.Args, untouched.
func rejectCaseAliases(value any, t reflect.Type, path string) error {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Struct:
		object, ok := value.(map[string]any)
		if !ok {
			return nil
		}
		fields := argumentFields(t, map[reflect.Type]bool{})
		names := fieldNames(fields)
		slices.Sort(names)
		keys := objectKeys(object)
		slices.Sort(keys)
		for _, key := range keys {
			childPath := fmt.Sprintf("%s[%q]", path, key)
			if fieldTypes, exact := fields[key]; exact {
				for _, fieldType := range fieldTypes {
					err := rejectCaseAliases(object[key], fieldType, childPath)
					if err != nil {
						return err
					}
				}
				continue
			}
			for _, name := range names {
				if strings.EqualFold(key, name) {
					return fmt.Errorf(
						"%s differs only in case from declared field %q",
						childPath,
						name,
					)
				}
			}
		}
	case reflect.Slice, reflect.Array:
		if t == reflect.TypeFor[json.RawMessage]() {
			return nil
		}
		items, ok := value.([]any)
		if !ok {
			return nil
		}
		for i, item := range items {
			err := rejectCaseAliases(
				item,
				t.Elem(),
				fmt.Sprintf("%s[%d]", path, i),
			)
			if err != nil {
				return err
			}
		}
	case reflect.Map:
		object, ok := value.(map[string]any)
		if !ok {
			return nil
		}
		keys := objectKeys(object)
		slices.Sort(keys)
		for _, key := range keys {
			if err := rejectCaseAliases(
				object[key],
				t.Elem(),
				fmt.Sprintf("%s[%q]", path, key),
			); err != nil {
				return err
			}
		}
	}
	return nil
}

func argumentFields(
	t reflect.Type,
	seen map[reflect.Type]bool,
) map[string][]reflect.Type {
	fields := map[string][]reflect.Type{}
	if seen[t] {
		return fields
	}
	seen[t] = true
	defer delete(seen, t)
	for field := range t.Fields() {
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if field.Anonymous && name == "" {
			base := field.Type
			for base.Kind() == reflect.Pointer {
				base = base.Elem()
			}
			if base.Kind() == reflect.Struct {
				for embedded, types := range argumentFields(base, seen) {
					fields[embedded] = append(fields[embedded], types...)
				}
				continue
			}
		}
		if !field.IsExported() {
			continue
		}
		if name == "" {
			name = field.Name
		}
		fields[name] = append(fields[name], field.Type)
	}
	return fields
}

func fieldNames(fields map[string][]reflect.Type) []string {
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	return names
}

func objectKeys(object map[string]any) []string {
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	return keys
}
