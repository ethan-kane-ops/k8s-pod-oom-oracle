package cgroup

import (
	"math"
	"reflect"
	"testing"
)

func TestParseUint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		want    uint64
		wantErr bool
	}{
		{name: "plain value", input: "1024", want: 1024},
		{name: "trailing newline", input: "1024\n", want: 1024},
		{name: "surrounding whitespace", input: "  1024  \n", want: 1024},
		{name: "zero", input: "0\n", want: 0},
		{name: "empty body is zero", input: "", want: 0},
		{name: "whitespace only is zero", input: "\n\n", want: 0},
		{name: "max uint64", input: "18446744073709551615", want: math.MaxUint64},
		{name: "negative rejected", input: "-1", wantErr: true},
		{name: "non-numeric rejected", input: "max", wantErr: true},
		{name: "overflow rejected", input: "18446744073709551616", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := ParseUint([]byte(tt.input))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseUint(%q) = %d, want error", tt.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseUint(%q) error = %v", tt.input, err)
			}
			if got != tt.want {
				t.Errorf("ParseUint(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseLimit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		want    uint64
		wantErr bool
	}{
		{name: "v2 max keyword", input: "max\n", want: Unlimited},
		{name: "v2 numeric limit", input: "536870912\n", want: 536870912},
		{name: "v1 page counter max is unlimited", input: "9223372036854771712\n", want: Unlimited},
		{name: "above page counter max is unlimited", input: "18446744073709551615\n", want: Unlimited},
		{name: "just below page counter max is a real limit", input: "9223372036854771711\n", want: 9223372036854771711},
		{name: "zero limit", input: "0\n", want: 0},
		{name: "garbage rejected", input: "unlimited\n", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := ParseLimit([]byte(tt.input))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseLimit(%q) = %d, want error", tt.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseLimit(%q) error = %v", tt.input, err)
			}
			if got != tt.want {
				t.Errorf("ParseLimit(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseKeyValue(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  map[string]uint64
	}{
		{
			name:  "memory.events",
			input: "low 0\nhigh 12\nmax 3\noom 2\noom_kill 1\n",
			want:  map[string]uint64{"low": 0, "high": 12, "max": 3, "oom": 2, "oom_kill": 1},
		},
		{
			name:  "memory.stat subset",
			input: "anon 1048576\nfile 524288\nkernel 65536\n",
			want:  map[string]uint64{"anon": 1048576, "file": 524288, "kernel": 65536},
		},
		{
			name:  "empty body",
			input: "",
			want:  map[string]uint64{},
		},
		{
			name:  "blank lines skipped",
			input: "anon 100\n\n\nfile 200\n",
			want:  map[string]uint64{"anon": 100, "file": 200},
		},
		{
			name:  "unparseable values skipped but siblings kept",
			input: "anon 100\nbroken notanumber\nfile 200\n",
			want:  map[string]uint64{"anon": 100, "file": 200},
		},
		{
			name:  "keyless lines skipped",
			input: "anon 100\nnovalue\nfile 200\n",
			want:  map[string]uint64{"anon": 100, "file": 200},
		},
		{
			name:  "negative values skipped",
			input: "anon 100\npgmajfault -5\n",
			want:  map[string]uint64{"anon": 100},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := ParseKeyValue([]byte(tt.input))
			if err != nil {
				t.Fatalf("ParseKeyValue() error = %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ParseKeyValue() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParsePSI(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		want    PSI
		wantErr bool
	}{
		{
			name:  "both rows present",
			input: "some avg10=1.50 avg60=0.75 avg300=0.10 total=123456\nfull avg10=0.50 avg60=0.25 avg300=0.05 total=7890\n",
			want: PSI{
				Some: PSILine{Avg10: 1.50, Avg60: 0.75, Avg300: 0.10, Total: 123456},
				Full: PSILine{Avg10: 0.50, Avg60: 0.25, Avg300: 0.05, Total: 7890},
			},
		},
		{
			name:  "root cgroup omits full row",
			input: "some avg10=0.00 avg60=0.00 avg300=0.00 total=0\n",
			want:  PSI{},
		},
		{
			name:  "unknown scope ignored",
			input: "partial avg10=9.99 avg60=9.99 avg300=9.99 total=1\nsome avg10=1.00 avg60=2.00 avg300=3.00 total=4\n",
			want:  PSI{Some: PSILine{Avg10: 1, Avg60: 2, Avg300: 3, Total: 4}},
		},
		{
			name:  "unknown field ignored",
			input: "some avg10=1.00 avg60=2.00 avg300=3.00 avg900=4.00 total=5\n",
			want:  PSI{Some: PSILine{Avg10: 1, Avg60: 2, Avg300: 3, Total: 5}},
		},
		{
			name:  "empty body",
			input: "",
			want:  PSI{},
		},
		{
			name:    "malformed average rejected",
			input:   "some avg10=notafloat avg60=0.00 avg300=0.00 total=0\n",
			wantErr: true,
		},
		{
			name:    "malformed total rejected",
			input:   "some avg10=0.00 avg60=0.00 avg300=0.00 total=abc\n",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := ParsePSI([]byte(tt.input))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParsePSI(%q) = %+v, want error", tt.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParsePSI() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("ParsePSI() = %+v, want %+v", got, tt.want)
			}
		})
	}
}
