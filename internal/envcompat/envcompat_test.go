package envcompat

import "testing"

func TestLegacyFallback(t *testing.T) {
	env := map[string]string{"PLUMBLINE_SERVER": "https://old", "TELLPROOF_DATA_DIR": "/new", "PLUMBLINE_DATA_DIR": "/old", "OTHER": "x"}
	get := Wrap(func(k string) string { return env[k] })
	for k, want := range map[string]string{"TELLPROOF_SERVER": "https://old", "TELLPROOF_DATA_DIR": "/new", "TELLPROOF_NONE": "", "OTHER": "x"} {
		if got := get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}
