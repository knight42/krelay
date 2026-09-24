package kube

import (
	"context"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestWaitPodGone(t *testing.T) {
	// The fake clientset does not implement the watch-list protocol (its
	// watches never send the initial-events bookmark), so the reflector's
	// cache would never sync; force the plain list+watch path.
	t.Setenv("KUBE_FEATURE_WatchListClient", "false")

	const (
		ns      = "default"
		jobName = "krelay-server-abcde"
		podName = "krelay-server-abcde-xyz"
	)
	serverPod := func(phase corev1.PodPhase) *corev1.Pod {
		return &corev1.Pod{
			Namespace: ns,
			Name:      podName,
			Labels:    map[string]string{"job-name": jobName},
			Status:    corev1.PodStatus{Phase: phase},
		}
	}

	testCases := map[string]struct {
		objs       []runtime.Object
		mutate     func(t *testing.T, cs *fake.Clientset)
		wantReason string
		wantErr    bool
	}{
		"pod deleted": {
			objs: []runtime.Object{serverPod(corev1.PodRunning)},
			mutate: func(t *testing.T, cs *fake.Clientset) {
				if err := cs.CoreV1().Pods(ns).Delete(t.Context(), podName, metav1.DeleteOptions{}); err != nil {
					t.Errorf("delete pod: %v", err)
				}
			},
			wantReason: "deleted",
		},
		"pod failed": {
			objs: []runtime.Object{serverPod(corev1.PodRunning)},
			mutate: func(t *testing.T, cs *fake.Clientset) {
				if _, err := cs.CoreV1().Pods(ns).UpdateStatus(t.Context(), serverPod(corev1.PodFailed), metav1.UpdateOptions{}); err != nil {
					t.Errorf("update pod status: %v", err)
				}
			},
			wantReason: "terminated: Failed",
		},
		"pod already failed at start": {
			objs:       []runtime.Object{serverPod(corev1.PodFailed)},
			wantReason: "terminated: Failed",
		},
		"pod already gone at start": {
			wantReason: "deleted",
		},
		"other pod ignored, context ends": {
			objs: []runtime.Object{
				serverPod(corev1.PodRunning),
				&corev1.Pod{
					Namespace: ns,
					Name:      "krelay-server-abcde-other",
					Labels:    map[string]string{"job-name": jobName},
					Status:    corev1.PodStatus{Phase: corev1.PodRunning},
				},
			},
			mutate: func(t *testing.T, cs *fake.Clientset) {
				if err := cs.CoreV1().Pods(ns).Delete(t.Context(), "krelay-server-abcde-other", metav1.DeleteOptions{}); err != nil {
					t.Errorf("delete pod: %v", err)
				}
			},
			wantErr: true,
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			cs := fake.NewClientset(tc.objs...)
			// The fake watch is a live tail with no resourceVersion replay: a
			// mutation before the reflector's watch is registered would be
			// missed forever. Gate mutations on the first Watch call.
			watchStarted := make(chan struct{})
			var once sync.Once
			cs.PrependWatchReactor("pods", func(action k8stesting.Action) (bool, watch.Interface, error) {
				w, err := cs.Tracker().Watch(action.GetResource(), action.GetNamespace())
				if err != nil {
					return false, nil, err
				}
				once.Do(func() { close(watchStarted) })
				return true, w, nil
			})

			timeout := 10 * time.Second
			if tc.wantErr {
				timeout = time.Second
			}
			ctx, cancel := context.WithTimeout(t.Context(), timeout)
			defer cancel()

			sj := &ServerJob{cs: cs, Namespace: ns, Name: jobName, PodName: podName}
			type result struct {
				reason string
				err    error
			}
			resCh := make(chan result, 1)
			go func() {
				reason, err := sj.WaitPodGone(ctx)
				resCh <- result{reason, err}
			}()

			if tc.mutate != nil {
				select {
				case <-watchStarted:
				case <-ctx.Done():
					t.Fatal("watch was never established")
				}
				tc.mutate(t, cs)
			}

			res := <-resCh
			if tc.wantErr {
				if res.err == nil {
					t.Fatalf("WaitPodGone() = (%q, nil), want error", res.reason)
				}
				return
			}
			if res.err != nil {
				t.Fatalf("WaitPodGone() error: %v", res.err)
			}
			if res.reason != tc.wantReason {
				t.Fatalf("WaitPodGone() reason = %q, want %q", res.reason, tc.wantReason)
			}
		})
	}
}
