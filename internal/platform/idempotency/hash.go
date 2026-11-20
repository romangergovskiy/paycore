package idempotency

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
)

// Hash is SHA-256 of endpoint, path values, and canonical JSON body.
// Headers are not included (concurrency.md §4.1).
func Hash(endpoint string, pathValues []string, canonicalBody []byte) []byte {
	h := sha256.New()
	_, _ = h.Write([]byte(endpoint))
	_, _ = h.Write([]byte{0})
	for _, v := range pathValues {
		_, _ = h.Write([]byte(v))
		_, _ = h.Write([]byte{0})
	}
	_, _ = h.Write(canonicalBody)
	return h.Sum(nil)
}

// CanonicalJSON re-encodes body with object keys sorted and no extra
// whitespace. Numbers stay json.Number so money amounts are not float64.
func CanonicalJSON(body []byte) ([]byte, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return []byte{}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("canonical json: %w", err)
	}
	return marshalCanonical(v)
}

func marshalCanonical(v any) ([]byte, error) {
	switch t := v.(type) {
	case map[string]any:
		keys := slices.Sorted(maps.Keys(t))
		buf := byteBuffer{'{'}
		for i, k := range keys {
			if i > 0 {
				buf = append(buf, ',')
			}
			kb, err := json.Marshal(k)
			if err != nil {
				return nil, err
			}
			buf = append(buf, kb...)
			buf = append(buf, ':')
			vb, err := marshalCanonical(t[k])
			if err != nil {
				return nil, err
			}
			buf = append(buf, vb...)
		}
		buf = append(buf, '}')
		return buf, nil
	case []any:
		buf := byteBuffer{'['}
		for i, el := range t {
			if i > 0 {
				buf = append(buf, ',')
			}
			vb, err := marshalCanonical(el)
			if err != nil {
				return nil, err
			}
			buf = append(buf, vb...)
		}
		buf = append(buf, ']')
		return buf, nil
	default:
		return json.Marshal(t)
	}
}

type byteBuffer []byte
