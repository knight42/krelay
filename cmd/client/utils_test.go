package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseTargetsFile(t *testing.T) {
	testCases := map[string]struct {
		input     string
		expect    []target
		expectErr string
	}{
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
		"invalid ip": {
			input:     `ip/1.2.3 8080`,
			expectErr: "invalid IP address",
		},
	}
	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			got, err := parseTargetsFile(strings.NewReader(tc.input), "")

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
