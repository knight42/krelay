package kube

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/spf13/pflag"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/knight42/krelay/pkg/constants"
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
	flags.StringVar(&f.serverImage, "server.image", "ghcr.io/knight42/krelay-server:v0.0.4", "The krelay-server image to use.")
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

func (f *Flags) buildServerPod() (*corev1.Pod, error) {
	origPod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:    metav1.NamespaceDefault,
			GenerateName: constants.ServerName + "-",
			Labels: map[string]string{
				"app.kubernetes.io/name": constants.ServerName,
				"app":                    constants.ServerName,
			},
			Annotations: map[string]string{
				"cluster-autoscaler.kubernetes.io/safe-to-evict": "true",
			},
		},
		Spec: corev1.PodSpec{
			AutomountServiceAccountToken: toPtr(false),
			EnableServiceLinks:           toPtr(false),
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot: toPtr(true),
			},
			Containers: []corev1.Container{
				{
					Name:            constants.ServerName,
					Image:           f.serverImage,
					ImagePullPolicy: corev1.PullAlways,
					SecurityContext: &corev1.SecurityContext{
						ReadOnlyRootFilesystem:   toPtr(true),
						AllowPrivilegeEscalation: toPtr(false),
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
	if len(f.patch) == 0 && len(f.patchFile) == 0 {
		return &origPod, nil
	}

	var patchBytes []byte
	if len(f.patch) > 0 {
		patchBytes = []byte(f.patch)
	} else if len(f.patchFile) > 0 {
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

	return patched, nil
}

func (f *Flags) RunServerPod(ctx context.Context) (*ServerPod, error) {
	restCfg, err := f.ToRESTConfig()
	if err != nil {
		return nil, err
	}
	cs, err := f.ToClientSet()
	if err != nil {
		return nil, err
	}

	svrPod, err := f.buildServerPod()
	if err != nil {
		return nil, err
	}

	l := slog.With(slog.String("namespace", svrPod.Namespace))
	l.Info("Creating krelay-server")
	createdPod, err := cs.CoreV1().Pods(svrPod.Namespace).Create(ctx, svrPod, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("create krelay-server pod: %w", err)
	}

	err = ensureServerPodIsRunning(ctx, cs, createdPod.Namespace, createdPod.Name)
	if err != nil {
		return nil, fmt.Errorf("ensure krelay-server is running: %w", err)
	}
	l.Info("krelay-server is running", slog.String("pod", createdPod.Name))

	restClient, err := rest.RESTClientFor(restCfg)
	if err != nil {
		return nil, err
	}

	req := restClient.Post().
		Resource("pods").
		Namespace(svrPod.Namespace).Name(createdPod.Name).
		SubResource("portforward")

	dialer, err := createDialer(restCfg, req.URL())
	if err != nil {
		return nil, fmt.Errorf("create dialer: %w", err)
	}

	l.Info("Creating port-forward stream to krelay-server pod")
	streamConn, _, err := dialer.Dial(constants.PortForwardProtocolV1Name)
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}

	ret := &ServerPod{
		cs:         cs,
		pod:        createdPod,
		streamConn: streamConn,
	}
	return ret, nil
}

type ServerPod struct {
	cs         kubernetes.Interface
	pod        *corev1.Pod
	streamConn httpstream.Connection
}

func (p *ServerPod) StreamConn() httpstream.Connection {
	return p.streamConn
}

func (p *ServerPod) Close() error {
	_ = p.streamConn.Close()
	removeServerPod(p.cs, p.pod.Namespace, p.pod.Name, time.Minute)
	return nil
}
