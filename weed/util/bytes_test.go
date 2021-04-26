package util

import "testing"

func TestParseMinFreeSpace(t *testing.T) {
	tests := []struct {
		in    string
		ok    bool
		value float32
	}{
		{in: "42", ok: true, value: 42},
		{in: "-1", ok: false, value: 0},
		{in: "101", ok: false, value: 0},
		{in: "100B", ok: false, value: 0},
		{in: "100Ki", ok: true, value: 100 * 1024},
		{in: "100GiB", ok: true, value: 100 * 1024 * 1024 * 1024},
		{in: "42M", ok: true, value: 42 * 1000 * 1000},
	}

	for _, p := range tests {
		got, err := ParseMinFreeSpace(p.in)
		if p.ok != (err == nil) {
			t.Errorf("failed to test %v", p.in)
		}
		if p.ok && err == nil && got != p.value {
			t.Errorf("failed to test %v", p.in)
		}
	}
}

func TestByteParsing(t *testing.T) {
	tests := []struct {
		in  string
		exp uint64
	}{
		{"42", 42},
		{"42MB", 42000000},
		{"42MiB", 44040192},
		{"42mb", 42000000},
		{"42mib", 44040192},
		{"42MIB", 44040192},
		{"42 MB", 42000000},
		{"42 MiB", 44040192},
		{"42 mb", 42000000},
		{"42 mib", 44040192},
		{"42 MIB", 44040192},
		{"42.5MB", 42500000},
		{"42.5MiB", 44564480},
		{"42.5 MB", 42500000},
		{"42.5 MiB", 44564480},
		// No need to say B
		{"42M", 42000000},
		{"42Mi", 44040192},
		{"42m", 42000000},
		{"42mi", 44040192},
		{"42MI", 44040192},
		{"42 M", 42000000},
		{"42 Mi", 44040192},
		{"42 m", 42000000},
		{"42 mi", 44040192},
		{"42 MI", 44040192},
		{"42.5M", 42500000},
		{"42.5Mi", 44564480},
		{"42.5 M", 42500000},
		{"42.5 Mi", 44564480},
		// Bug #42
		{"1,005.03 MB", 1005030000},
		// Large testing, breaks when too much larger than
		// this.
		{"12.5 EB", uint64(12.5 * float64(EByte))},
		{"12.5 E", uint64(12.5 * float64(EByte))},
		{"12.5 EiB", uint64(12.5 * float64(EiByte))},
	}

	for _, p := range tests {
		got, err := ParseBytes(p.in)
		if err != nil {
			t.Errorf("Couldn't parse %v: %v", p.in, err)
		}
		if got != p.exp {
			t.Errorf("Expected %v for %v, got %v",
				p.exp, p.in, got)
		}
	}
}
