package sse

import (
	"bufio"
	"io"
	"strings"
)

// Event is a parsed Server-Sent Event.
type Event struct {
	Type string
	Data string
	ID   string
}

// Parser reads SSE events from a stream.
type Parser struct {
	s *bufio.Scanner
}

func NewParser(r io.Reader) *Parser {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 64*1024), 1024*1024)
	s.Split(scanSSELines)
	return &Parser{s: s}
}

// scanSSELines is a bufio.SplitFunc that honours all three line endings
// defined by the SSE spec: CRLF, LF, and bare CR.
func scanSSELines(data []byte, atEOF bool) (advance int, token []byte, err error) {
	for i := 0; i < len(data); i++ {
		switch data[i] {
		case '\r':
			if i+1 < len(data) {
				if data[i+1] == '\n' {
					return i + 2, data[:i], nil // CRLF
				}
				return i + 1, data[:i], nil // bare CR
			}
			if atEOF {
				return i + 1, data[:i], nil // bare CR at end of stream
			}
			return 0, nil, nil // need one more byte to decide
		case '\n':
			return i + 1, data[:i], nil // LF
		}
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil // unterminated final line
	}
	return 0, nil, nil
}

// Next reads and returns the next complete SSE event.
// Returns io.EOF when the stream is exhausted.
func (p *Parser) Next() (*Event, error) {
	ev := &Event{}
	var dataLines []string

	for {
		scanned := p.s.Scan()
		if !scanned {
			if err := p.s.Err(); err != nil {
				return nil, err
			}
			if len(dataLines) > 0 {
				ev.Data = strings.Join(dataLines, "\n")
				return ev, nil
			}
			return nil, io.EOF
		}

		line := p.s.Text()

		if line == "" {
			if len(dataLines) > 0 || ev.Type != "" {
				ev.Data = strings.Join(dataLines, "\n")
				return ev, nil
			}
			continue
		}

		if strings.HasPrefix(line, ":") {
			continue
		}

		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")

		switch field {
		case "event":
			ev.Type = value
		case "data":
			dataLines = append(dataLines, value)
		case "id":
			ev.ID = value
		}
	}
}

// Chan drains r into a channel of events; closes on EOF or context cancellation.
func Chan(r io.Reader, done <-chan struct{}) <-chan *Event {
	ch := make(chan *Event, 64)
	go func() {
		defer close(ch)
		p := NewParser(r)
		for {
			ev, err := p.Next()
			if err != nil {
				return
			}
			select {
			case ch <- ev:
			case <-done:
				return
			}
		}
	}()
	return ch
}
