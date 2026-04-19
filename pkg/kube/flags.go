package kube

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/spf13/pflag"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/knight42/krelay/pkg/constants"
)

const (
	ttlSecondsAfterFinished int32 = 10
	jobBackoffLimit         int32 = 0
)

type Flags struct {
	cf *genericclioptions.ConfigFlags

	restCfg *rest.Config

	// serverImage is the image to use for the krelay-server.
	serverImage string
	// patch is the literal MergePatch to be applied to the krelay-server pod.
	patch string
	// patchFile is the file containing the MergePatch to be applied to the krelay-server pod.
	patchFile string
}

func NewFlags() *Flags {
	return &Flags{
		cf: genericclioptions.NewConfigFlags(true),
	}
}

func (f *Flags) AddFlags(flags *pflag.FlagSet) {
	flags.StringVar(f.cf.KubeConfig, "kubeconfig", *f.cf.KubeConfig, "Path to the kubeconfig file to use for CLI requests.")
	flags.StringVarP(f.cf.Namespace, "namespace", "n", *f.cf.Namespace, "If present, the namespace scope for this CLI request")
	flags.StringVar(f.cf.Context, "context", *f.cf.Context, "The name of the kubeconfig context to use")
	flags.StringVar(f.cf.ClusterName, "cluster", *f.cf.ClusterName, "The name of the kubeconfig cluster to use")

	flags.StringVarP(&f.patch, "patch", "p", "", "The merge patch to be applied to the krelay-server pod.")
	flags.StringVar(&f.patchFile, "patch-file", "", "A file containing a merge patch to be applied to the krelay-server pod.")
	flags.StringVar(&f.serverImage, "server.image", "ghcr.io/knight42/krelay-server:v0.0.5", "The krelay-server image to use.")
}

func (f *Flags) GetNamespace() (string, bool, error) {
	return f.cf.ToRawKubeConfigLoader().Namespace()
}

func (f *Flags) ToRESTConfig() (*rest.Config, error) {
	if f.restCfg != nil {
		return f.restCfg, nil
	}
	var err error
	f.restCfg, err = f.cf.ToRESTConfig()
	if err != nil {
		return nil, err
	}
	setKubernetesDefaults(f.restCfg)
	return f.restCfg, nil
}

func (f *Flags) ToClientSet() (kubernetes.Interface, error) {
	restCfg, err := f.ToRESTConfig()
	if err != nil {
		return nil, err
	}
	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, err
	}
	return cs, nil
}

func (f *Flags) ToResourceBuilder() *resource.Builder {
	return resource.NewBuilder(f.cf)
}

func (f *Flags) buildServerJob() (*batchv1.Job, error) {
	podLabels := map[string]string{
		"app.kubernetes.io/name": constants.ServerName,
		"app":                    constants.ServerName,
	}
	origPod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: metav1.NamespaceDefault,
			Labels:    podLabels,
			Annotations: map[string]string{
				"cluster-autoscaler.kubernetes.io/safe-to-evict": "true",
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever,
			AutomountServiceAccountToken: new(false),
			EnableServiceLinks:           new(false),
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot: new(true),
			},
			Containers: []corev1.Container{
				{
					Name:            constants.ServerName,
					Image:           f.serverImage,
					ImagePullPolicy: corev1.PullAlways,
					SecurityContext: &corev1.SecurityContext{
						ReadOnlyRootFilesystem:   new(true),
						AllowPrivilegeEscalation: new(false),
					},
				},
			},
			TopologySpreadConstraints: []corev1.TopologySpreadConstraint{
				{
					MaxSkew:           1,
					TopologyKey:       "kubernetes.io/hostname",
					WhenUnsatisfiable: corev1.ScheduleAnyway,
					LabelSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"app": constants.ServerName,
						},
					},
				},
			},
		},
	}

	if len(f.patch) > 0 || len(f.patchFile) > 0 {
		var patchBytes []byte
		if len(f.patch) > 0 {
			patchBytes = []byte(f.patch)
		} else {
			var err error
			patchBytes, err = os.ReadFile(f.patchFile)
			if err != nil {
				return nil, fmt.Errorf("read file: %w", err)
			}
		}
		patched, err := patchPod(patchBytes, origPod)
		if err != nil {
			return nil, fmt.Errorf("patch server pod: %w", err)
		}
		origPod = *patched
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:    origPod.Namespace,
			GenerateName: constants.ServerName + "-",
			Labels:       podLabels,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            new(jobBackoffLimit),
			TTLSecondsAfterFinished: new(ttlSecondsAfterFinished),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      origPod.Labels,
					Annotations: origPod.Annotations,
				},
				Spec: origPod.Spec,
			},
		},
	}
	return job, nil
}

func (f *Flags) RunServerJob(ctx context.Context) (*ServerJob, error) {
	restCfg, err := f.ToRESTConfig()
	if err != nil {
		return nil, err
	}
	cs, err := f.ToClientSet()
	if err != nil {
		return nil, err
	}

	svrJob, err := f.buildServerJob()
	if err != nil {
		return nil, err
	}

	l := slog.With(slog.String("namespace", svrJob.Namespace))
	l.Info("Creating krelay-server job")
	createdJob, err := cs.BatchV1().Jobs(svrJob.Namespace).Create(ctx, svrJob, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("create krelay-server job: %w", err)
	}
	// Clean up the Job if anything below fails — the caller never gets a
	// ServerJob handle to call Close() on.
	cleanup := func() { removeServerJob(cs, createdJob.Namespace, createdJob.Name, time.Minute) }

	podName, err := waitForServerJobPod(ctx, cs, createdJob.Namespace, createdJob.Name)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("wait for krelay-server pod: %w", err)
	}
	l.Info("krelay-server is running", slog.String("job", createdJob.Name), slog.String("pod", podName))

	restClient, err := rest.RESTClientFor(restCfg)
	if err != nil {
		cleanup()
		return nil, err
	}

	req := restClient.Post().
		Resource("pods").
		Namespace(createdJob.Namespace).Name(podName).
		SubResource("portforward")

	dialer, err := createDialer(restCfg, req.URL())
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("create dialer: %w", err)
	}

	l.Info("Creating port-forward stream to krelay-server pod")
	streamConn, _, err := dialer.Dial(constants.PortForwardProtocolV1Name)
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}

	return &ServerJob{
		cs:         cs,
		job:        createdJob,
		streamConn: streamConn,
	}, nil
}

type ServerJob struct {
	cs         kubernetes.Interface
	job        *batchv1.Job
	streamConn httpstream.Connection
}

func (p *ServerJob) StreamConn() httpstream.Connection {
	return p.streamConn
}

func (p *ServerJob) Close() error {
	_ = p.streamConn.Close()
	removeServerJob(p.cs, p.job.Namespace, p.job.Name, time.Minute)
	return nil
}
