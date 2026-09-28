package main

import (
	"fmt"
	"strconv"
	"time"

	"cloud.google.com/go/civil"
)

// The library deals in Go values only; turning them into text and back is
// this CLI's job, since it is the one talking to a person.

// formatValue renders a value for the terminal. time.Duration and the civil
// types print in their own String forms (1h2m3.5s, 2024-06-15T13:30:00).
func formatValue(v any) string {
	if s, ok := v.(string); ok {
		return strconv.Quote(s)
	}
	return fmt.Sprint(v)
}

// parseLike parses text as a value of the same Go type as like, which is the
// symbol's current value. Durations take Go's syntax ("1h23m45.678s"), times of
// day "15:04:05.999", dates "2006-01-02", and date-times "2006-01-02T15:04:05".
func parseLike(like any, text string) (any, error) {
	switch like.(type) {
	case bool:
		return strconv.ParseBool(text)
	case int8:
		n, err := strconv.ParseInt(text, 0, 8)
		return int8(n), err
	case int16:
		n, err := strconv.ParseInt(text, 0, 16)
		return int16(n), err
	case int32:
		n, err := strconv.ParseInt(text, 0, 32)
		return int32(n), err
	case int64:
		return strconv.ParseInt(text, 0, 64)
	case uint8:
		n, err := strconv.ParseUint(text, 0, 8)
		return uint8(n), err
	case uint16:
		n, err := strconv.ParseUint(text, 0, 16)
		return uint16(n), err
	case uint32:
		n, err := strconv.ParseUint(text, 0, 32)
		return uint32(n), err
	case uint64:
		return strconv.ParseUint(text, 0, 64)
	case float32:
		f, err := strconv.ParseFloat(text, 32)
		return float32(f), err
	case float64:
		return strconv.ParseFloat(text, 64)
	case string:
		return text, nil
	case time.Duration:
		return time.ParseDuration(text)
	case civil.Time:
		return civil.ParseTime(text)
	case civil.Date:
		return civil.ParseDate(text)
	case civil.DateTime:
		return civil.ParseDateTime(text)
	}
	return nil, fmt.Errorf("cannot write a %T from the command line; write one of its members instead", like)
}
