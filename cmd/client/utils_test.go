package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseTargetsFile(t *testing.T) {
	testCases := map[string]struct {
		input            string
		defaultNamespace string

		expect    []target
		expectErr string
	}{
		// valid cases
		"normal": {
			input: `bar 53@udp 53@tcp`,
			expect: []target{
				{
					resource: "bar",
					ports:    []string{"53@udp", "53@tcp"},
				},
			},
		},
		"comment & empty line": {
			input: `
# comment
ip/1.2.3.4 8080
// comment
host/google.com 443@tcp

pod/foo 8000 8001
`,
			expect: []target{
				{
					resource: "ip/1.2.3.4",
					ports:    []string{"8080"},
				},
				{
					resource: "host/google.com",
					ports:    []string{"443@tcp"},
				},
				{
					resource: "pod/foo",
					ports:    []string{"8000", "8001"},
				},
			},
		},
		"different namespaces": {
			defaultNamespace: "foo",
			input: `
-n bar1 svc/q 8000
host/q.com 8000 9000:9001
-n=bar2 svc/q 8000
svc/q 8000
`,
			expect: []target{
				{
					resource:  "svc/q",
					ports:     []string{"8000"},
					namespace: "bar1",
				},
				{
					resource:  "host/q.com",
					ports:     []string{"8000", "9000:9001"},
					namespace: "foo",
				},
				{
					resource:  "svc/q",
					ports:     []string{"8000"},
					namespace: "bar2",
				},
				{
					resource:  "svc/q",
					ports:     []string{"8000"},
					namespace: "foo",
				},
			},
		},

		// invalid cases
		"invalid ip": {
			input:     `ip/1.2.3 8080`,
			expectErr: "invalid IP address",
		},
		"unknown flag": {
			input:     `-invalid-flag foo 8080`,
			expectErr: "flag provided but not defined: -invalid-flag",
		},
		"missing value for -n flag": {
			input:     `-n foo 8080`,
			expectErr: "invalid syntax",
		},
	}
	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			got, err := parseTargetsFile(strings.NewReader(tc.input), tc.defaultNamespace)

			if len(tc.expectErr) == 0 {
				require.NoError(t, err)
				require.Equal(t, tc.expect, got)
				return
			}

			require.Error(t, err)
			t.Logf("error: %v", err)
			require.ErrorContains(t, err, tc.expectErr)
		})
	}
}
