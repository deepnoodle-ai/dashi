package tool

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// parseUniqueArguments keeps json.Number precision for schema validation and
// refuses duplicate object keys. encoding/json merges repeated object values
// into structs, while schema validation sees only the final JSON object.
func parseUniqueArguments(raw json.RawMessage) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	value, err := parseUniqueValue(decoder, "$")
	if err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("unexpected trailing input")
		}
		return nil, err
	}
	return value, nil
}

func parseUniqueValue(decoder *json.Decoder, path string) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	switch token {
	case json.Delim('{'):
		object := map[string]any{}
		for decoder.More() {
			token, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			key := token.(string)
			childPath := fmt.Sprintf("%s[%q]", path, key)
			if _, exists := object[key]; exists {
				return nil, fmt.Errorf("duplicate JSON object key %s", childPath)
			}
			object[key], err = parseUniqueValue(decoder, childPath)
			if err != nil {
				return nil, err
			}
		}
		_, err := decoder.Token()
		return object, err
	case json.Delim('['):
		var items []any
		for decoder.More() {
			item, err := parseUniqueValue(
				decoder,
				fmt.Sprintf("%s[%d]", path, len(items)),
			)
			if err != nil {
				return nil, err
			}
			items = append(items, item)
		}
		_, err := decoder.Token()
		return items, err
	default:
		return token, nil
	}
}
