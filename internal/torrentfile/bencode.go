package torrentfile

import (
	"errors"
	"fmt"
	"strconv"
)

// The bencode format is small enough to parse directly: four types, each
// introduced by its own first byte.
//
// The one subtlety is the info dictionary. Its hash is taken over the bytes
// as they were written, not over a re-encoding of what was decoded — a
// re-encoding can differ in key order or in the spelling of a number, and
// the hash would then be of a torrent that does not exist. So the parser
// hands back the raw slice of the input every value was parsed from.

var errTruncated = errors.New("the file ends in the middle of a value")

// entry is one key-value pair of a dictionary.
//
// The value is the decoded form; raw is the slice of the input it was
// decoded from.
type entry struct {
	value any
	raw   []byte
}

// parser walks a bencoded buffer.
type parser struct {
	buf []byte
	pos int
}

// parseValue decodes the value at the current position and returns it along
// with the bytes it occupied.
func (p *parser) parseValue() (any, []byte, error) {
	if p.pos >= len(p.buf) {
		return nil, nil, errTruncated
	}
	switch c := p.buf[p.pos]; {
	case c == 'i':
		return p.parseInt()
	case c == 'l':
		return p.parseList()
	case c == 'd':
		return p.parseDict()
	case c >= '0' && c <= '9':
		return p.parseString()
	default:
		return nil, nil, fmt.Errorf("unexpected byte %q at offset %d", c, p.pos)
	}
}

// parseInt reads "i<number>e".
func (p *parser) parseInt() (any, []byte, error) {
	start := p.pos
	p.pos++ // 'i'

	end := -1
	for i := p.pos; i < len(p.buf); i++ {
		if p.buf[i] == 'e' {
			end = i
			break
		}
	}
	if end < 0 {
		return nil, nil, errTruncated
	}

	digits := string(p.buf[p.pos:end])
	p.pos = end + 1

	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return nil, nil, fmt.Errorf("bad integer %q: %w", digits, err)
	}
	return n, p.buf[start:p.pos], nil
}

// parseString reads "<length>:<bytes>".
//
// The length is a byte count, not a character count: a name in any encoding
// is stored as the bytes it is.
func (p *parser) parseString() (any, []byte, error) {
	start := p.pos

	colon := -1
	for i := p.pos; i < len(p.buf); i++ {
		if p.buf[i] == ':' {
			colon = i
			break
		}
		if p.buf[i] < '0' || p.buf[i] > '9' {
			return nil, nil, fmt.Errorf("bad string length at offset %d", p.pos)
		}
	}
	if colon < 0 {
		return nil, nil, errTruncated
	}

	n, err := strconv.Atoi(string(p.buf[p.pos:colon]))
	if err != nil {
		return nil, nil, fmt.Errorf("bad string length: %w", err)
	}
	p.pos = colon + 1

	if n < 0 || p.pos+n > len(p.buf) {
		return nil, nil, errTruncated
	}
	b := p.buf[p.pos : p.pos+n]
	p.pos += n

	return b, p.buf[start:p.pos], nil
}

// parseList reads "l<value>…e".
func (p *parser) parseList() (any, []byte, error) {
	start := p.pos
	p.pos++ // 'l'

	items := []any{}
	for {
		if p.pos >= len(p.buf) {
			return nil, nil, errTruncated
		}
		if p.buf[p.pos] == 'e' {
			p.pos++
			break
		}
		v, _, err := p.parseValue()
		if err != nil {
			return nil, nil, err
		}
		items = append(items, v)
	}

	return items, p.buf[start:p.pos], nil
}

// parseDict reads "d<key><value>…e".
//
// Keys are byte strings; a key that is not one is a malformed file, not a
// dictionary this parser should guess about.
func (p *parser) parseDict() (any, []byte, error) {
	start := p.pos
	p.pos++ // 'd'

	m := map[string]entry{}
	for {
		if p.pos >= len(p.buf) {
			return nil, nil, errTruncated
		}
		if p.buf[p.pos] == 'e' {
			p.pos++
			break
		}

		key, _, err := p.parseValue()
		if err != nil {
			return nil, nil, err
		}
		kb, ok := key.([]byte)
		if !ok {
			return nil, nil, fmt.Errorf("a dictionary key at offset %d is not a string", p.pos)
		}

		value, raw, err := p.parseValue()
		if err != nil {
			return nil, nil, err
		}
		m[string(kb)] = entry{value: value, raw: raw}
	}

	return m, p.buf[start:p.pos], nil
}

// asBytes returns the value of a byte string.
func asBytes(v any) ([]byte, bool) {
	b, ok := v.([]byte)
	return b, ok
}

// asInt returns the value of an integer.
func asInt(v any) (int64, bool) {
	n, ok := v.(int64)
	return n, ok
}

// asDict returns the value of a dictionary.
func asDict(v any) (map[string]entry, bool) {
	m, ok := v.(map[string]entry)
	return m, ok
}

// asList returns the value of a list.
func asList(v any) ([]any, bool) {
	l, ok := v.([]any)
	return l, ok
}
