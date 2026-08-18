package mcppolicy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const maxJSONDepth = 64

var (
	errDuplicateJSONKey = errors.New("duplicate JSON object key")
	errInvalidJSON      = errors.New("invalid JSON document")
)

// decodeUniqueJSONObject rejects duplicate object keys at every depth before
// decoding. encoding/json otherwise accepts the final duplicate, which lets a
// policy look benign to one parser while enforcing a different value in
// another implementation.
func decodeUniqueJSONObject(data []byte) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := consumeJSONValue(decoder, 0); err != nil {
		return nil, err
	}
	if token, err := decoder.Token(); err != io.EOF || token != nil {
		return nil, errInvalidJSON
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil || object == nil {
		return nil, errInvalidJSON
	}
	return object, nil
}

func consumeJSONValue(decoder *json.Decoder, depth int) error {
	if depth > maxJSONDepth {
		return fmt.Errorf("%w: nesting exceeds limit", errInvalidJSON)
	}
	token, err := decoder.Token()
	if err != nil {
		return errInvalidJSON
	}
	delimiter, composite := token.(json.Delim)
	if !composite {
		switch token.(type) {
		case nil, bool, string, json.Number:
			return nil
		default:
			return errInvalidJSON
		}
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, keyErr := decoder.Token()
			if keyErr != nil {
				return errInvalidJSON
			}
			key, ok := keyToken.(string)
			if !ok {
				return errInvalidJSON
			}
			if _, exists := seen[key]; exists {
				return errDuplicateJSONKey
			}
			seen[key] = struct{}{}
			if valueErr := consumeJSONValue(decoder, depth+1); valueErr != nil {
				return valueErr
			}
		}
		closing, closeErr := decoder.Token()
		if closeErr != nil || closing != json.Delim('}') {
			return errInvalidJSON
		}
		return nil
	case '[':
		for decoder.More() {
			if valueErr := consumeJSONValue(decoder, depth+1); valueErr != nil {
				return valueErr
			}
		}
		closing, closeErr := decoder.Token()
		if closeErr != nil || closing != json.Delim(']') {
			return errInvalidJSON
		}
		return nil
	default:
		return errInvalidJSON
	}
}

func rawString(value json.RawMessage) (string, bool) {
	var decoded string
	if err := json.Unmarshal(value, &decoded); err != nil {
		return "", false
	}
	return decoded, true
}

func rawBool(value json.RawMessage) (bool, bool) {
	var decoded bool
	if err := json.Unmarshal(value, &decoded); err != nil {
		return false, false
	}
	return decoded, true
}

func rawStringSlice(value json.RawMessage) ([]string, bool) {
	var decoded []string
	if err := json.Unmarshal(value, &decoded); err != nil || decoded == nil {
		return nil, false
	}
	return decoded, true
}

func rawStringMap(value json.RawMessage) (map[string]string, bool) {
	var decoded map[string]string
	if err := json.Unmarshal(value, &decoded); err != nil || decoded == nil {
		return nil, false
	}
	return decoded, true
}
