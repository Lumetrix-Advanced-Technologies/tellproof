package jcs

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

type jcsVectorFile struct {
	Cases []struct {
		Name     string `json:"name"`
		Input    string `json:"input"`
		Expected string `json:"expected"`
	} `json:"cases"`
	MustReject []struct {
		Name  string `json:"name"`
		Input string `json:"input"`
	} `json:"must_reject"`
}

func loadJCSVectors(t testing.TB) jcsVectorFile {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "spec", "jcs-vectors.json"))
	if err != nil {
		t.Fatalf("read jcs vectors: %v", err)
	}
	var v jcsVectorFile
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("decode jcs vectors: %v", err)
	}
	if len(v.Cases) == 0 || len(v.MustReject) == 0 {
		t.Fatal("jcs vectors file has no cases")
	}
	return v
}

func TestJCSVectors(t *testing.T) {
	for _, c := range loadJCSVectors(t).Cases {
		t.Run(c.Name, func(t *testing.T) {
			got, err := Canonicalize([]byte(c.Input))
			if err != nil {
				t.Fatalf("Canonicalize(%q) error: %v", c.Input, err)
			}
			if string(got) != c.Expected {
				t.Errorf("Canonicalize(%q)\n got %q\nwant %q", c.Input, got, c.Expected)
			}
		})
	}
}

func TestJCSVectorsMustReject(t *testing.T) {
	for _, c := range loadJCSVectors(t).MustReject {
		t.Run(c.Name, func(t *testing.T) {
			if got, err := Parse([]byte(c.Input)); err == nil {
				t.Errorf("Parse(%q) = %#v, want an error", c.Input, got)
			}
		})
	}
}

func TestJCSParseRejects(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  error // nil: any error will do
	}{
		{"duplicate keys", `{"a":1,"a":2}`, ErrDuplicateKey},
		{"duplicate keys nested", `[{"x":{"k":1,"k":1}}]`, ErrDuplicateKey},
		{"duplicate keys spelt with escapes", `{"a":1,"a":2}`, ErrDuplicateKey},
		{"lone high surrogate", `["\ud800"]`, ErrInvalidUnicode},
		{"lone low surrogate", `["\udc00"]`, ErrInvalidUnicode},
		{"high surrogate then non-surrogate escape", `["\ud800A"]`, ErrInvalidUnicode},
		{"high surrogate then text", `["\ud800x"]`, ErrInvalidUnicode},
		{"high surrogate at end of string", `["\uD83D"]`, ErrInvalidUnicode},
		{"lone surrogate in key", `{"\ud800":1}`, ErrInvalidUnicode},
		{"invalid UTF-8 byte", "[\"\xff\"]", ErrInvalidUnicode},
		{"UTF-8 encoded surrogate", "[\"\xed\xa0\x80\"]", ErrInvalidUnicode},
		{"number overflows float64", `[1e400]`, ErrNonFinite},
		{"negative number overflows float64", `[-1e400]`, ErrNonFinite},
		{"NaN", `[NaN]`, nil},
		{"Infinity", `[Infinity]`, nil},
		{"leading zero", `[01]`, nil},
		{"missing colon", `{"a" 1}`, nil},
		{"missing comma", `[1 2]`, nil},
		{"trailing comma in array", `[1,]`, nil},
		{"trailing comma in object", `{"a":1,}`, nil},
		{"raw control character in string", "[\"\x01\"]", nil},
		{"single quotes", `['a']`, nil},
		{"trailing value", `[] []`, ErrTrailingData},
		{"trailing garbage", `{}x`, nil},
		{"empty input", ``, nil},
		{"unclosed array", `[1`, nil},
		{"too deep", strings.Repeat("[", maxDepth+1) + strings.Repeat("]", maxDepth+1), ErrTooDeep},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Parse([]byte(tt.input))
			if err == nil {
				t.Fatalf("Parse(%q) = %#v, want an error", tt.input, got)
			}
			if tt.want != nil && !errors.Is(err, tt.want) {
				t.Errorf("Parse(%q) error = %v, want errors.Is %v", tt.input, err, tt.want)
			}
		})
	}
}

func TestJCSParseAccepts(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  any
	}{
		{"escaped backslash before u", `["\\ud800"]`, []any{`\ud800`}},
		{"surrogate pair escape", `["😀"]`, []any{"😀"}},
		{"uppercase hex surrogate pair", `["😀"]`, []any{"😀"}},
		{"escaped quote then u", `["\"A"]`, []any{`"A`}},
		{"max depth", strings.Repeat("[", maxDepth) + strings.Repeat("]", maxDepth), nestedArrays(maxDepth)},
		{"top-level scalar", `  42  `, 42.0},
		{"top-level null", `null`, nil},
		{"negative zero", `-0`, math.Copysign(0, -1)},
		{"empty key", `{"":1}`, map[string]any{"": 1.0}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Parse([]byte(tt.input))
			if err != nil {
				t.Fatalf("Parse(%q) error: %v", tt.input, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Parse(%q) = %#v, want %#v", tt.input, got, tt.want)
			}
		})
	}
}

// nestedArrays returns n arrays nested inside each other, innermost empty.
func nestedArrays(n int) any {
	v := []any{}
	for i := 1; i < n; i++ {
		v = []any{v}
	}
	return v
}

func TestJCSMarshal(t *testing.T) {
	tests := []struct {
		name string
		in   any
		want string
	}{
		{"nil", nil, `null`},
		{"bools", []any{true, false}, `[true,false]`},
		{"negative zero", math.Copysign(0, -1), `0`},
		{"html characters are not escaped", "<a href='x'>&</a>", `"<a href='x'>&</a>"`},
		{"line and paragraph separators are not escaped", "  ", "\"  \""},
		{"DEL is not escaped", "\x7f", "\"\x7f\""},
		{"control characters", "\x00\x08\x09\x0a\x0b\x0c\x0d\x1f", `"\u0000\b\t\n\u000b\f\r\u001f"`},
		{"keys sorted by UTF-16 code unit", map[string]any{"｡": 1.0, "\U0001F600": 2.0, "a": 0.0, "B": 3.0, "": 4.0},
			"{\"\":4,\"B\":3,\"a\":0,\"\U0001F600\":2,\"｡\":1}"},
		{"nested empty containers", map[string]any{"a": []any{}, "b": map[string]any{}}, `{"a":[],"b":{}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Marshal(tt.in)
			if err != nil {
				t.Fatalf("Marshal error: %v", err)
			}
			if string(got) != tt.want {
				t.Errorf("Marshal\n got %q\nwant %q", got, tt.want)
			}
		})
	}
}

func TestJCSMarshalRejects(t *testing.T) {
	tests := []struct {
		name string
		in   any
		want error
	}{
		{"NaN", math.NaN(), ErrNonFinite},
		{"+Inf", math.Inf(1), ErrNonFinite},
		{"-Inf nested", map[string]any{"x": []any{math.Inf(-1)}}, ErrNonFinite},
		{"invalid UTF-8 string", "\xff", ErrInvalidUnicode},
		{"invalid UTF-8 key", map[string]any{"\xff": 1.0}, ErrInvalidUnicode},
		{"unsupported type int", 1, ErrUnsupportedType},
		{"unsupported type struct", struct{}{}, ErrUnsupportedType},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Marshal(tt.in)
			if !errors.Is(err, tt.want) {
				t.Errorf("Marshal = %q, %v; want errors.Is %v", got, err, tt.want)
			}
		})
	}
}

// es-numbers.json holds float64 bit patterns and the ECMAScript String(x) output,
// generated by Node (testdata/gen-es-numbers.mjs).
func TestJCSNumbersMatchECMAScript(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "es-numbers.json"))
	if err != nil {
		t.Fatalf("read es-numbers.json: %v", err)
	}
	var pairs [][2]string
	if err := json.Unmarshal(b, &pairs); err != nil {
		t.Fatalf("decode es-numbers.json: %v", err)
	}
	if len(pairs) < 1000 {
		t.Fatalf("es-numbers.json has %d cases, want at least 1000", len(pairs))
	}
	for _, p := range pairs {
		u, err := strconv.ParseUint(p[0], 16, 64)
		if err != nil {
			t.Fatalf("bad bits %q: %v", p[0], err)
		}
		f := math.Float64frombits(u)
		got, err := Marshal(f)
		if err != nil {
			t.Errorf("Marshal(%v) [bits %s] error: %v", f, p[0], err)
			continue
		}
		if string(got) != p[1] {
			t.Errorf("Marshal(%v) [bits %s] = %s, want %s", f, p[0], got, p[1])
		}
	}
}

// The pass vectors' payloads were produced by the reference signer; JCS must
// reproduce them byte for byte or no Go-issued digest will match.
func TestJCSPassVectorPayloadsCanonical(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "spec", "vectors", "pass", "*.dsse.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no pass vectors found (err %v)", err)
	}
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			var env struct {
				Payload string `json:"payload"`
			}
			if err := json.Unmarshal(b, &env); err != nil {
				t.Fatal(err)
			}
			payload, err := base64.StdEncoding.DecodeString(env.Payload)
			if err != nil {
				t.Fatal(err)
			}
			got, err := Canonicalize(payload)
			if err != nil {
				t.Fatalf("Canonicalize payload: %v", err)
			}
			if !bytes.Equal(got, payload) {
				t.Errorf("payload does not re-serialize byte-identically (%d bytes in, %d out)", len(payload), len(got))
			}
		})
	}
}

func FuzzJCSRoundTrip(f *testing.F) {
	for _, c := range loadJCSVectors(f).Cases {
		f.Add([]byte(c.Input))
	}
	for _, s := range []string{`{"a":1,"a":2}`, `["\ud800"]`, `["\\ud800"]`, `[1e400]`, `{"<":" "}`, `[-0.0,1e-7,1e21]`} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		v, err := Parse(data)
		if err != nil {
			return
		}
		// Anything we accept, encoding/json accepts with the same meaning.
		var std any
		if err := json.Unmarshal(data, &std); err != nil {
			t.Fatalf("Parse accepted %q but encoding/json rejects it: %v", data, err)
		}
		if !reflect.DeepEqual(v, std) {
			t.Fatalf("Parse(%q) = %#v, encoding/json = %#v", data, v, std)
		}
		out, err := Marshal(v)
		if err != nil {
			t.Fatalf("Marshal of parsed %q: %v", data, err)
		}
		v2, err := Parse(out)
		if err != nil {
			t.Fatalf("Parse of canonical %q: %v", out, err)
		}
		if !reflect.DeepEqual(v, v2) {
			t.Fatalf("round trip changed value: %#v -> %q -> %#v", v, out, v2)
		}
		out2, err := Marshal(v2)
		if err != nil {
			t.Fatalf("second Marshal: %v", err)
		}
		if !bytes.Equal(out, out2) {
			t.Fatalf("canonical form is not stable: %q then %q", out, out2)
		}
	})
}

func FuzzJCSNumber(f *testing.F) {
	for _, x := range []float64{0, 1, -1, 0.1, 1e21, 1e-7, 5e-324, math.MaxFloat64, 123456789012345680000} {
		f.Add(math.Float64bits(x))
	}
	f.Fuzz(func(t *testing.T, bits uint64) {
		x := math.Float64frombits(bits)
		out, err := Marshal(x)
		if math.IsNaN(x) || math.IsInf(x, 0) {
			if !errors.Is(err, ErrNonFinite) {
				t.Fatalf("Marshal(%v) error = %v, want ErrNonFinite", x, err)
			}
			return
		}
		if err != nil {
			t.Fatalf("Marshal(%v): %v", x, err)
		}
		back, err := strconv.ParseFloat(string(out), 64)
		if err != nil {
			t.Fatalf("Marshal(%v) = %q is not a number: %v", x, out, err)
		}
		if back != x {
			t.Fatalf("Marshal(%v) = %q parses back as %v", x, out, back)
		}
	})
}
