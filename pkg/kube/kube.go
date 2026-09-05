// Package kube manages the krelay-server Job in the user's cluster.
package kube

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	watchtools "k8s.io/client-go/tools/watch"

	"github.com/knight42/krelay/pkg/constants"
)

// ServerOptions configures the krelay-server Job.
type ServerOptions struct {
	Namespace  string
	Image      string
	PullPolicy string
	// Args are passed to the krelay-server container.
	Args []string
	// NodeName, when non-empty, schedules the pod on this specific node with
	// hostPID and privileged access, enabling the --ssh mode that uses nsenter.
	NodeName string
}

// ServerJob is a handle to a running krelay-server Job.
type ServerJob struct {
	cs        kubernetes.Interface
	Namespace string
	Name      string
	PodName   string
}

// RunServerJob creates the krelay-server Job, waits for its pod to run, and
// returns a handle. The Job is removed on any error after creation.
func RunServerJob(ctx context.Context, cs kubernetes.Interface, opts ServerOptions) (*ServerJob, error) {
	job := buildServerJob(opts)
	slog.Info("Creating krelay-server job", slog.String("namespace", opts.Namespace))
	createdJob, err := cs.BatchV1().Jobs(opts.Namespace).Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("create krelay-server job: %w", err)
	}
	sj := &ServerJob{cs: cs, Namespace: createdJob.Namespace, Name: createdJob.Name}

	podName, err := waitForServerPod(ctx, cs, createdJob.Namespace, createdJob.Name)
	if err != nil {
		sj.Close()
		return nil, fmt.Errorf("wait for krelay-server pod: %w", err)
	}
	sj.PodName = podName
	slog.Info("krelay-server is running", slog.String("job", createdJob.Name), slog.String("pod", podName))
	return sj, nil
}

// ReadToken follows the server pod's logs until the connection token line
// appears and returns the token.
func (sj *ServerJob) ReadToken(ctx context.Context) (string, error) {
	stream, err := sj.cs.CoreV1().Pods(sj.Namespace).GetLogs(sj.PodName, &corev1.PodLogOptions{
		Container: constants.ServerName,
		Follow:    true,
	}).Stream(ctx)
	if err != nil {
		return "", fmt.Errorf("stream krelay-server logs: %w", err)
	}
	defer stream.Close()

	// Unblock the scanner below when ctx is canceled.
	stop := context.AfterFunc(ctx, func() { stream.Close() })
	defer stop()

	scanner := bufio.NewScanner(stream)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if token, ok := strings.CutPrefix(line, constants.TokenPrefix); ok {
			return token, nil
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("read krelay-server logs: %w", err)
	}
	return "", fmt.Errorf("krelay-server logs ended without a %s line", constants.TokenPrefix)
}

// Close deletes the server Job and its pods. It uses its own timeout so that
// cleanup still happens when the caller's context is already canceled.
func (sj *ServerJob) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	err := sj.cs.BatchV1().Jobs(sj.Namespace).Delete(ctx, sj.Name, metav1.DeleteOptions{
		PropagationPolicy: new(metav1.DeletePropagationBackground),
	})
	if err != nil {
		slog.Warn("Fail to remove krelay-server job", slog.String("job", sj.Name), slog.Any("error", err))
	}
}

func buildServerJob(opts ServerOptions) *batchv1.Job {
	podLabels := map[string]string{
		"app.kubernetes.io/name": constants.ServerName,
		"app":                    constants.ServerName,
	}
	spec := corev1.PodSpec{
		RestartPolicy:                corev1.RestartPolicyNever,
		AutomountServiceAccountToken: new(false),
		EnableServiceLinks:           new(false),
		Tolerations: []corev1.Toleration{
			{Key: "CriticalAddonsOnly", Operator: corev1.TolerationOpExists},
			{Effect: corev1.TaintEffectNoExecute, Operator: corev1.TolerationOpExists},
		},
		Containers: []corev1.Container{{
			Name:            constants.ServerName,
			Image:           opts.Image,
			ImagePullPolicy: corev1.PullPolicy(opts.PullPolicy),
			Args:            opts.Args,
		}},
	}
	if opts.NodeName != "" {
		spec.NodeName = opts.NodeName
		spec.HostPID = true
		spec.Containers[0].SecurityContext = &corev1.SecurityContext{
			Privileged: new(true),
		}
	} else {
		spec.SecurityContext = &corev1.PodSecurityContext{
			RunAsNonRoot: new(true),
		}
		spec.Containers[0].SecurityContext = &corev1.SecurityContext{
			ReadOnlyRootFilesystem:   new(true),
			AllowPrivilegeEscalation: new(false),
		}
	}
	return &batchv1.Job{
		GenerateName: constants.ServerName + "-",
		Labels:       podLabels,
		Spec: batchv1.JobSpec{
			BackoffLimit:            new(int32(0)),
			TTLSecondsAfterFinished: new(int32(10)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: podLabels,
					Annotations: map[string]string{
						"cluster-autoscaler.kubernetes.io/safe-to-evict": "true",
					},
				},
				Spec: spec,
			},
		},
	}
}

func waitForServerPod(ctx context.Context, cs kubernetes.Interface, namespace, jobName string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	selector := "job-name=" + jobName
	lw := &cache.ListWatch{
		ListWithContextFunc: func(ctx context.Context, options metav1.ListOptions) (runtime.Object, error) {
			options.LabelSelector = selector
			return cs.CoreV1().Pods(namespace).List(ctx, options)
		},
		WatchFuncWithContext: func(ctx context.Context, options metav1.ListOptions) (watch.Interface, error) {
			options.LabelSelector = selector
			return cs.CoreV1().Pods(namespace).Watch(ctx, options)
		},
	}

	var podName string
	_, err := watchtools.UntilWithSync(ctx, lw, &corev1.Pod{}, nil, func(ev watch.Event) (bool, error) {
		pod, ok := ev.Object.(*corev1.Pod)
		if !ok {
			return false, nil
		}
		switch pod.Status.Phase {
		case corev1.PodRunning:
			podName = pod.Name
			return true, nil
		case corev1.PodFailed, corev1.PodSucceeded:
			return false, fmt.Errorf("krelay-server pod %s terminated: %s", pod.Name, pod.Status.Phase)
		default:
			return false, nil
		}
	})
	return podName, err
}
