// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License;
// you may not use this file except in compliance with the Elastic License.

package httpjson

import (
	"encoding/json"
	"fmt"
	"strings"
)

// parse returns array of string values from JSON string
//
// Examples:
// input:
//   rawJSON={"a":[{"b":"b_value_1"},{"b":"b_value_2"},{"b":"b_value_3"}]}
//   key=a.#.b
// output:
//   ["a_value_1", "a_value_2"]
// input:
//   rawJSON=[{"a":"a_value_1"},{"a":"a_value_2"}]
//   key=#.a
// output:
//   ["a_value_1", "a_value_2"]
func parse(rawJSON, key string) ([]string, error) {
	var data interface{}
	err := json.Unmarshal([]byte(rawJSON), &data)
	if err != nil {
		return nil, fmt.Errorf("cannot unmarshal input data: %w", err)
	}
	values, err := parseInterface(data, key)
	if err != nil {
		return nil, fmt.Errorf("error while parsing JSON: %w", err)
	}
	return values, nil
}

// parseInterface returns array of string values from JSON data interface
//
// Examples:
// input:
// 	 data=
//   {
//   	"a": {
//   		"b": "b_value",
//   	},
//   }
//   key=a..b
// output:
//   ["b_value"]
// input:
//   data=
// 	 {
// 	 	"a": [
// 	 		{
// 	 			"b":"b_value_1"
// 	 		},
// 	 		{
// 	 			"b":"b_value_2"
// 	 		}
// 	 	]
// 	 }
//   key=a.#.b
// output:
//   ["b_value_1", "b_value_2"]
func parseInterface(data interface{}, key string) (values []string, err error) {
	splitKey := strings.Split(key, ".")
	for i, key := range splitKey {
		switch key {
		case "#":
			// '#' means it's an array and we want to get the
			// next key from each object in the array
			tmp, ok := data.([]interface{})
			if !ok {
				return nil, fmt.Errorf("error while parsing json: %w", err)
			}
			tmp, err = getArrayValue(splitKey[i+1], tmp)
			if err != nil {
				return nil, fmt.Errorf("error while parsing json: %w", err)
			}

			// interate over []interface{}
			for _, el := range tmp {
				if value, ok := el.(string); ok {
					values = append(values, value)
				} else {
					joinKey := strings.Join(splitKey[i+2:], ".")
					final, err := parseInterface(el, joinKey)
					if err != nil {
						return nil, fmt.Errorf("error while parsing json: %w", err)
					}
					values = append(values, final...)
				}
			}

		default:
			// default assumes it's a JSON object, so it expects a
			// map[string]interface{}
			data, err = getMapValue(key, data)
			if err != nil {
				return nil, fmt.Errorf("json is: %w", err)
			}
			if len(splitKey) == i+1 {
				if value, ok := data.(string); ok {
					values = append(values, value)
				}
			}
		}
		if key == "#" {
			break
		}
	}
	return values, nil
}

// getArrayValue if array is a []interface{} it looks for key in all the jsons
// of array and returns its result. If data is not a []interface{} or the
// key is not present, an error is returned and the returned value is nil
//
// Examples:
// input:
//   key=a
//   array=
//   [
//   	{
//   		"a": "a_value_1",
//   	},
//   	{
//   		"a": "a_value_2",
//   	},
//   ]
// output:
//   ["a_value_1", "a_value_2"]
func getArrayValue(key string, array []interface{}) ([]interface{}, error) {
	result := make([]interface{}, len(array))
	for i, el := range array {
		m, ok := el.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("invalid key")

		}
		value, ok := m[key]
		if !ok {
			return nil, fmt.Errorf("key field not found")
		}
		result[i] = value
	}
	return result, nil
}

// getMapValue if data is a map[string]interface{} it looks
// for key and returns its value. If data is not a map[string]interface{}
// or the key is not present, an error is returned and the returned value
// is nil
//
// Examples:
// input:
//   key=a
//   data=
//   {
//   	"a": "a_value",
//   }
// output:
//   a_value
// input:
//   key=a
//   data=
//   {
//   	"a": {
//   		"b": "b_value",
//   	},
//   }
// output:
//   {
//   	"b": "b_value",
//   }
func getMapValue(key string, data interface{}) (interface{}, error) {
	if m, ok := data.(map[string]interface{}); ok {
		if value, ok := m[key]; ok {
			return value, nil
		}
	}
	return nil, fmt.Errorf("key field not found")
}
