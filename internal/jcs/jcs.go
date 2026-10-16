// Package jcs implements the JSON Canonicalization Scheme (RFC 8785) and a
// strict parser for its input, per docs/architecture.md §2.1.
//
// Values are the types encoding/json produces when decoding into any: nil, bool,
// float64, string, []any and map[string]any. To canonicalize a Go struct, encode
// it with encoding/json and pass the bytes to Canonicalize.
package jcs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strconv"
	"unicode/utf16"
	"unicode/utf8"
)

// maxDepth bounds nesting in untrusted input. Packs and batches nest a few levels.
const maxDepth = 1000

var (
	ErrDuplicateKey    = errors.New("jcs: duplicate object key")
	ErrInvalidUnicode  = errors.New("jcs: invalid Unicode")
	ErrNonFinite       = errors.New("jcs: number is NaN or infinite")
	ErrTrailingData    = errors.New("jcs: unexpected data after the JSON value")
	ErrTooDeep         = errors.New("jcs: nesting too deep")
	ErrUnsupportedType = errors.New("jcs: unsupported type")
)

// Canonicalize parses data strictly and returns its RFC 8785 canonical form.
func Canonicalize(data []byte) ([]byte, error) {
	v, err := Parse(data)
	if err != nil {
		return nil, err
	}
	return Marshal(v)
}

// Parse decodes one JSON value. Beyond RFC 8259 it rejects what I-JSON
// (RFC 7493) forbids: duplicate object keys, invalid UTF-8, unpaired surrogate
// escapes, and numbers that do not fit a float64.
func Parse(data []byte) (any, error) {
	// encoding/json replaces both of these with U+FFFD instead of failing.
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("%w: input is not valid UTF-8", ErrInvalidUnicode)
	}
	if err := checkSurrogateEscapes(data); err != nil {
		return nil, err
	}

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	v, err := parseValue(dec, 0)
	if err != nil {
		return nil, err
	}
	switch _, err := dec.Token(); {
	case errors.Is(err, io.EOF):
		return v, nil
	case err != nil:
		return nil, fmt.Errorf("jcs: %w", err)
	default:
		return nil, ErrTrailingData
	}
}

func parseValue(dec *json.Decoder, depth int) (any, error) {
	tok, err := dec.Token()
	if errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("jcs: %w", io.ErrUnexpectedEOF)
	}
	if err != nil {
		return nil, fmt.Errorf("jcs: %w", err)
	}
	switch t := tok.(type) {
	case json.Delim:
		if depth >= maxDepth {
			return nil, fmt.Errorf("%w: more than %d levels", ErrTooDeep, maxDepth)
		}
		switch t {
		case '{':
			return parseObject(dec, depth+1)
		case '[':
			return parseArray(dec, depth+1)
		}
		return nil, fmt.Errorf("jcs: unexpected %q", rune(t))
	case json.Number:
		f, err := strconv.ParseFloat(string(t), 64)
		if err != nil {
			if errors.Is(err, strconv.ErrRange) { // overflow only; underflow rounds to 0
				return nil, fmt.Errorf("%w: %s overflows float64", ErrNonFinite, t)
			}
			return nil, fmt.Errorf("jcs: number %s: %w", t, err)
		}
		return f, nil
	case string, bool, nil:
		return t, nil
	}
	return nil, fmt.Errorf("jcs: unexpected token %v", tok)
}

func parseObject(dec *json.Decoder, depth int) (any, error) {
	m := map[string]any{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("jcs: %w", err)
		}
		key, ok := tok.(string)
		if !ok {
			return nil, fmt.Errorf("jcs: object key is %v, not a string", tok)
		}
		if _, dup := m[key]; dup {
			return nil, fmt.Errorf("%w %q", ErrDuplicateKey, key)
		}
		v, err := parseValue(dec, depth)
		if err != nil {
			return nil, err
		}
		m[key] = v
	}
	if _, err := dec.Token(); err != nil { // the closing '}'
		return nil, fmt.Errorf("jcs: %w", err)
	}
	return m, nil
}

func parseArray(dec *json.Decoder, depth int) (any, error) {
	a := []any{}
	for dec.More() {
		v, err := parseValue(dec, depth)
		if err != nil {
			return nil, err
		}
		a = append(a, v)
	}
	if _, err := dec.Token(); err != nil { // the closing ']'
		return nil, fmt.Errorf("jcs: %w", err)
	}
	return a, nil
}

// checkSurrogateEscapes rejects \uXXXX escapes that encode an unpaired UTF-16
// surrogate. Only string contents are inspected; malformed JSON is left for the
// decoder to reject.
func checkSurrogateEscapes(data []byte) error {
	inString := false
	for i := 0; i < len(data); i++ {
		c := data[i]
		if !inString {
			if c == '"' {
				inString = true
			}
			continue
		}
		switch c {
		case '"':
			inString = false
		case '\\':
			if i+1 >= len(data) {
				return nil
			}
			if data[i+1] != 'u' {
				i++ // skip the escaped character, which may be '"' or '\'
				continue
			}
			u, ok := hex4(data, i+2)
			if !ok {
				return nil
			}
			switch {
			case utf16.IsSurrogate(rune(u)) && u < 0xDC00: // high surrogate
				lo, ok := uint16(0), false
				if i+7 < len(data) && data[i+6] == '\\' && data[i+7] == 'u' {
					lo, ok = hex4(data, i+8)
				}
				if !ok || lo < 0xDC00 || lo > 0xDFFF {
					return fmt.Errorf("%w: unpaired high surrogate \\u%04x", ErrInvalidUnicode, u)
				}
				i += 11
			case utf16.IsSurrogate(rune(u)): // low surrogate with no high before it
				return fmt.Errorf("%w: unpaired low surrogate \\u%04x", ErrInvalidUnicode, u)
			default:
				i += 5
			}
		}
	}
	return nil
}

// hex4 reads four hex digits at data[i:i+4].
func hex4(data []byte, i int) (uint16, bool) {
	if i+4 > len(data) {
		return 0, false
	}
	n, err := strconv.ParseUint(string(data[i:i+4]), 16, 16)
	if err != nil {
		return 0, false
	}
	return uint16(n), true
}

// Marshal returns the RFC 8785 canonical form of v.
func Marshal(v any) ([]byte, error) {
	return appendValue(nil, v)
}

func appendValue(b []byte, v any) ([]byte, error) {
	switch x := v.(type) {
	case nil:
		return append(b, "null"...), nil
	case bool:
		return strconv.AppendBool(b, x), nil
	case float64:
		return appendNumber(b, x)
	case string:
		return appendString(b, x)
	case []any:
		b = append(b, '[')
		for i, e := range x {
			if i > 0 {
				b = append(b, ',')
			}
			var err error
			if b, err = appendValue(b, e); err != nil {
				return nil, err
			}
		}
		return append(b, ']'), nil
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		slices.SortFunc(keys, compareUTF16)
		b = append(b, '{')
		for i, k := range keys {
			if i > 0 {
				b = append(b, ',')
			}
			var err error
			if b, err = appendString(b, k); err != nil {
				return nil, err
			}
			b = append(b, ':')
			if b, err = appendValue(b, x[k]); err != nil {
				return nil, err
			}
		}
		return append(b, '}'), nil
	}
	return nil, fmt.Errorf("%w %T", ErrUnsupportedType, v)
}

// compareUTF16 orders strings by their UTF-16 code units, as RFC 8785 §3.2.3
// requires. This differs from byte (and code point) order for characters above
// U+FFFF versus U+E000–U+FFFF.
func compareUTF16(a, b string) int {
	return slices.Compare(utf16.Encode([]rune(a)), utf16.Encode([]rune(b)))
}

// appendString escapes only '"', '\' and U+0000–U+001F (RFC 8785 §3.2.2.2).
func appendString(b []byte, s string) ([]byte, error) {
	if !utf8.ValidString(s) {
		return nil, fmt.Errorf("%w: string is not valid UTF-8", ErrInvalidUnicode)
	}
	const hexDigits = "0123456789abcdef"
	b = append(b, '"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"' || c == '\\':
			b = append(b, '\\', c)
		case c >= 0x20:
			b = append(b, c)
		case c == '\b':
			b = append(b, '\\', 'b')
		case c == '\t':
			b = append(b, '\\', 't')
		case c == '\n':
			b = append(b, '\\', 'n')
		case c == '\f':
			b = append(b, '\\', 'f')
		case c == '\r':
			b = append(b, '\\', 'r')
		default:
			b = append(b, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0xF])
		}
	}
	return append(b, '"'), nil
}

// appendNumber formats f as ECMAScript Number.prototype.toString does
// (RFC 8785 §3.2.2.3). strconv's 'e' format gives the shortest round-trip
// digits; only the layout differs from ES.
func appendNumber(b []byte, f float64) ([]byte, error) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return nil, ErrNonFinite
	}
	if f == 0 { // also -0
		return append(b, '0'), nil
	}
	if f < 0 {
		b = append(b, '-')
		f = -f
	}

	// e is "d.ddddde±XX": collect the digits and the decimal exponent.
	e := strconv.AppendFloat(nil, f, 'e', -1, 64)
	mark := bytes.IndexByte(e, 'e')
	exp, err := strconv.Atoi(string(e[mark+1:]))
	if err != nil {
		return nil, fmt.Errorf("jcs: format %v: %w", f, err)
	}
	digits := make([]byte, 0, mark)
	for _, c := range e[:mark] {
		if c != '.' {
			digits = append(digits, c)
		}
	}
	k := len(digits) // number of significant digits
	n := exp + 1     // position of the decimal point relative to the digits

	switch {
	case k <= n && n <= 21: // integer: digits then zeros
		b = append(b, digits...)
		for range n - k {
			b = append(b, '0')
		}
	case 0 < n && n <= 21: // point inside the digits
		b = append(b, digits[:n]...)
		b = append(b, '.')
		b = append(b, digits[n:]...)
	case -6 < n && n <= 0: // small: 0.000ddd
		b = append(b, '0', '.')
		for range -n {
			b = append(b, '0')
		}
		b = append(b, digits...)
	default: // exponent form: d[.ddd]e±x
		b = append(b, digits[0])
		if k > 1 {
			b = append(b, '.')
			b = append(b, digits[1:]...)
		}
		b = append(b, 'e')
		if n-1 >= 0 {
			b = append(b, '+')
		}
		b = strconv.AppendInt(b, int64(n-1), 10)
	}
	return b, nil
}
