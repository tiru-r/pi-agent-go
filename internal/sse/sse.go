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
	r *bufio.Reader
}

func NewParser(r io.Reader) *Parser {
	return &Parser{r: bufio.NewReader(r)}
}

// Next reads and returns the next complete SSE event.
// Returns io.EOF when the stream is exhausted.
func (p *Parser) Next() (*Event, error) {
	ev := &Event{}
	var dataLines []string

	for {
		line, err := p.r.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")

		if err != nil && err != io.EOF {
			return nil, err
		}

		if line == "" {
			// Empty line signals end of event.
			if len(dataLines) > 0 || ev.Type != "" {
				ev.Data = strings.Join(dataLines, "\n")
				return ev, nil
			}
			if err == io.EOF {
				return nil, io.EOF
			}
			continue
		}

		// Comment line — skip.
		if strings.HasPrefix(line, ":") {
			if err == io.EOF {
				return nil, io.EOF
			}
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

		if err == io.EOF {
			if len(dataLines) > 0 {
				ev.Data = strings.Join(dataLines, "\n")
				return ev, nil
			}
			return nil, io.EOF
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
