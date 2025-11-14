package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
					lisAddr:  "127.0.0.1",
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
					lisAddr:  "127.0.0.1",
				},
				{
					resource: "host/google.com",
					ports:    []string{"443@tcp"},
					lisAddr:  "127.0.0.1",
				},
				{
					resource: "pod/foo",
					ports:    []string{"8000", "8001"},
					lisAddr:  "127.0.0.1",
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
					lisAddr:   "127.0.0.1",
				},
				{
					resource:  "host/q.com",
					ports:     []string{"8000", "9000:9001"},
					namespace: "foo",
					lisAddr:   "127.0.0.1",
				},
				{
					resource:  "svc/q",
					ports:     []string{"8000"},
					namespace: "bar2",
					lisAddr:   "127.0.0.1",
				},
				{
					resource:  "svc/q",
					ports:     []string{"8000"},
					namespace: "foo",
					lisAddr:   "127.0.0.1",
				},
			},
		},
		"with listen address": {
			input: `
-l localhost svc/q 8000
--address 1.1.1.1 host/q.com 8000
`,
			expect: []target{
				{
					resource: "svc/q",
					ports:    []string{"8000"},
					lisAddr:  "localhost",
				},
				{
					resource: "host/q.com",
					ports:    []string{"8000"},
					lisAddr:  "1.1.1.1",
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
			expectErr: "unknown shorthand flag",
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

func TestPatchPod(t *testing.T) {
	testCases := map[string]struct {
		patch   string
		origPod corev1.Pod

		expected corev1.Pod
	}{
		"patch in json": {
			patch: `{"metadata": {"name": "foo"}}`,
			origPod: corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "bar",
					Namespace: metav1.NamespaceDefault,
				},
			},

			expected: corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "foo",
					Namespace: metav1.NamespaceDefault,
				},
			},
		},

		"patch in yaml": {
			patch: `
metadata:
  name: foo`,
			origPod: corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "bar",
					Namespace: metav1.NamespaceDefault,
				},
			},

			expected: corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "foo",
					Namespace: metav1.NamespaceDefault,
				},
			},
		},
	}
	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			got, err := patchPod([]byte(tc.patch), tc.origPod)
			require.NoError(t, err)
			require.Equal(t, tc.expected, *got)
		})
	}
}
