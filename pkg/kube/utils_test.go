package kube

import (
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

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
