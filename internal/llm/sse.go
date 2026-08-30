package llm

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

func scanSSE(r io.Reader, maxLine int, handle func(string, []byte) error, malformed func(string) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), maxLine)
	var data []string
	eventName := ""
	dispatch := func() error {
		if len(data) == 0 {
			eventName = ""
			return nil
		}
		payload := strings.Join(data, "\n")
		name := eventName
		data = nil
		eventName = ""
		return handle(name, []byte(payload))
	}
	for sc.Scan() {
		line := strings.TrimSuffix(sc.Text(), "\r")
		if line == "" {
			if err := dispatch(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, ok := strings.Cut(line, ":")
		if !ok {
			if malformed != nil {
				return malformed(line)
			}
			return fmt.Errorf("malformed SSE line %q", line)
		}
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			eventName = value
		case "data":
			data = append(data, value)
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return dispatch()
}
