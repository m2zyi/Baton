package adapters

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
)

type jsonlLine struct {
	number int
	data   []byte
}

// readJSONLines decodes complete JSONL records. A malformed final record is
// treated as a partially written tail only when the file does not end in a
// newline. A newline-terminated malformed record is a complete, corrupt record
// and is reported with its exact line number.
func readJSONLines(path string) ([]jsonlLine, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, nil
	}

	endsWithNewline := data[len(data)-1] == '\n'
	raw := bytes.Split(data, []byte{'\n'})
	last := len(raw) - 1
	if endsWithNewline {
		last--
	}

	lines := make([]jsonlLine, 0, last+1)
	for index := 0; index <= last; index++ {
		line := bytes.TrimSuffix(raw[index], []byte{'\r'})
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if !json.Valid(line) {
			if !endsWithNewline && index == last {
				break
			}
			return nil, fmt.Errorf("%s:%d: invalid JSON record", path, index+1)
		}
		lines = append(lines, jsonlLine{
			number: index + 1,
			data:   append([]byte(nil), line...),
		})
	}
	return lines, nil
}

func stableID(parts ...string) string {
	// IDs are composed by callers from native identifiers. Callers that have no
	// native identifier use eventIDFromContent instead.
	return joinNonEmpty(parts, ":")
}

func joinNonEmpty(parts []string, separator string) string {
	out := ""
	for _, part := range parts {
		if part == "" {
			continue
		}
		if out != "" {
			out += separator
		}
		out += part
	}
	return out
}
