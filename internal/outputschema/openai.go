package outputschema

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
)

const (
	openAIMaxDepth      = 5
	openAIMaxProperties = 100
	openAIMaxEnumValues = 250
)

var openAIKeywords = []string{
	"type", "properties", "required", "additionalProperties", "items",
	"enum", "description",
}

// CheckOpenAIStrict accepts a deliberately small subset of OpenAI's strict
// response-format dialect. Compile must have succeeded first. No rewrite is
// made: a caller can compare the admitted schema with the prepared body.
func CheckOpenAIStrict(raw json.RawMessage) error {
	var node any
	if err := json.Unmarshal(raw, &node); err != nil {
		return fmt.Errorf("output_schema: %w", err)
	}
	properties, enumValues := 0, 0
	return checkOpenAINode(node, "$", 1, true, &properties, &enumValues)
}

func checkOpenAINode(
	value any,
	path string,
	depth int,
	root bool,
	properties *int,
	enumValues *int,
) error {
	if depth > openAIMaxDepth {
		return fmt.Errorf("output_schema %s: nesting exceeds %d", path, openAIMaxDepth)
	}
	node, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("output_schema %s: boolean and non-object schemas are unsupported", path)
	}
	for _, key := range slices.Sorted(maps.Keys(node)) {
		if !slices.Contains(openAIKeywords, key) {
			return fmt.Errorf("output_schema %s.%s: keyword is unsupported", path, key)
		}
	}
	if description, exists := node["description"]; exists {
		if _, ok := description.(string); !ok {
			return fmt.Errorf("output_schema %s.description: must be a string", path)
		}
	}
	typeName, err := openAIType(node["type"], root)
	if err != nil {
		return fmt.Errorf("output_schema %s.type: %w", path, err)
	}
	if values, exists := node["enum"]; exists {
		enum, ok := values.([]any)
		if !ok || len(enum) == 0 {
			return fmt.Errorf("output_schema %s.enum: must be a nonempty array", path)
		}
		*enumValues += len(enum)
		if *enumValues > openAIMaxEnumValues {
			return fmt.Errorf("output_schema %s.enum: total exceeds %d", path, openAIMaxEnumValues)
		}
		for i, value := range enum {
			switch value.(type) {
			case nil, string, float64, bool:
			default:
				return fmt.Errorf("output_schema %s.enum[%d]: only scalar values are supported", path, i)
			}
		}
	}
	switch typeName {
	case "object":
		members, ok := node["properties"].(map[string]any)
		if !ok {
			return fmt.Errorf("output_schema %s.properties: object requires properties", path)
		}
		if node["additionalProperties"] != false {
			return fmt.Errorf("output_schema %s.additionalProperties: must be false", path)
		}
		required, ok := node["required"].([]any)
		if !ok || len(required) != len(members) {
			return fmt.Errorf("output_schema %s.required: must name every property exactly once", path)
		}
		seen := make(map[string]bool, len(required))
		for _, entry := range required {
			name, ok := entry.(string)
			if !ok || seen[name] {
				return fmt.Errorf("output_schema %s.required: must name every property exactly once", path)
			}
			if _, ok := members[name]; !ok {
				return fmt.Errorf("output_schema %s.required: unknown property %q", path, name)
			}
			seen[name] = true
		}
		if _, exists := node["items"]; exists {
			return fmt.Errorf("output_schema %s.items: only arrays may have items", path)
		}
		*properties += len(members)
		if *properties > openAIMaxProperties {
			return fmt.Errorf("output_schema %s.properties: total exceeds %d", path, openAIMaxProperties)
		}
		for _, name := range slices.Sorted(maps.Keys(members)) {
			child := members[name]
			if err := checkOpenAINode(
				child,
				fmt.Sprintf("%s.properties[%q]", path, name),
				depth+1,
				false,
				properties,
				enumValues,
			); err != nil {
				return err
			}
		}
	case "array":
		if _, exists := node["properties"]; exists {
			return fmt.Errorf("output_schema %s.properties: only objects may have properties", path)
		}
		if _, exists := node["required"]; exists {
			return fmt.Errorf("output_schema %s.required: only objects may have required", path)
		}
		if _, exists := node["additionalProperties"]; exists {
			return fmt.Errorf("output_schema %s.additionalProperties: only objects may set it", path)
		}
		child, exists := node["items"]
		if !exists {
			return fmt.Errorf("output_schema %s.items: array requires one item schema", path)
		}
		return checkOpenAINode(child, path+".items", depth+1, false, properties, enumValues)
	default:
		for _, key := range []string{"properties", "required", "additionalProperties", "items"} {
			if _, exists := node[key]; exists {
				return fmt.Errorf("output_schema %s.%s: keyword does not apply to %s", path, key, typeName)
			}
		}
	}
	return nil
}

func openAIType(value any, root bool) (string, error) {
	name, ok := value.(string)
	if !ok {
		values, isArray := value.([]any)
		if root || !isArray || len(values) != 2 {
			return "", fmt.Errorf("expected one type%s", rootSuffix(root))
		}
		first, firstOK := values[0].(string)
		second, secondOK := values[1].(string)
		if !firstOK || !secondOK || first == second {
			return "", fmt.Errorf("nullable type must contain one type and null")
		}
		switch {
		case first == "null":
			name = second
		case second == "null":
			name = first
		default:
			return "", fmt.Errorf("nullable type must contain null")
		}
	}
	if root && name != "object" {
		return "", fmt.Errorf("root must have type object")
	}
	if !slices.Contains([]string{
		"object", "array", "string", "integer", "number", "boolean", "null",
	}, name) {
		return "", fmt.Errorf("unsupported type %q", name)
	}
	return name, nil
}

func rootSuffix(root bool) string {
	if root {
		return " at the root"
	}
	return " or a nullable pair"
}
