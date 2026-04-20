//go:build e2e

package e2e

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

var (
	krelayBin    string
	kubeClient   kubernetes.Interface
	testNS       string
	svcClusterIP string
)

func TestMain(m *testing.M) {
	var code int
	defer func() { os.Exit(code) }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	repoRoot, err := findRepoRoot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "find repo root: %v\n", err)
		code = 1
		return
	}

	tmpDir, err := os.MkdirTemp("", "krelay-e2e-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create temp dir: %v\n", err)
		code = 1
		return
	}
	defer os.RemoveAll(tmpDir)

	krelayBin = filepath.Join(tmpDir, "krelay")
	fmt.Println("Building krelay binary...")
	buildCmd := exec.CommandContext(ctx, "go", "build", "-o", krelayBin, "./cmd/client")
	buildCmd.Dir = repoRoot
	buildCmd.Stdout = os.Stdout
	buildCmd.Stderr = os.Stderr
	if err := buildCmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "build krelay: %v\n", err)
		code = 1
		return
	}

	kubeClient, err = buildKubeClient()
	if err != nil {
		fmt.Fprintf(os.Stderr, "create k8s client: %v\n", err)
		code = 1
		return
	}

	testNS = fmt.Sprintf("krelay-e2e-%d", time.Now().UnixNano())
	fmt.Printf("Creating test namespace %s...\n", testNS)
	_, err = kubeClient.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: testNS},
	}, metav1.CreateOptions{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "create namespace: %v\n", err)
		code = 1
		return
	}
	defer func() {
		cleanupCtx := context.Background()
		cleanupServerPods(cleanupCtx)
		fmt.Printf("Deleting test namespace %s...\n", testNS)
		_ = kubeClient.CoreV1().Namespaces().Delete(cleanupCtx, testNS, metav1.DeleteOptions{})
	}()

	cleanupServerPods(ctx)

	fmt.Println("Deploying test fixtures...")
	if err := deployFixtures(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "deploy fixtures: %v\n", err)
		code = 1
		return
	}

	fmt.Println("Running tests...")
	code = m.Run()
}

func findRepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found")
		}
		dir = parent
	}
}

func buildKubeClient() (kubernetes.Interface, error) {
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		home, _ := os.UserHomeDir()
		kubeconfig = filepath.Join(home, ".kube", "config")
	}
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(config)
}

func nginxPodSpec() corev1.PodSpec {
	return corev1.PodSpec{
		Containers: []corev1.Container{
			{
				Name:  "nginx",
				Image: "nginx:alpine",
				Ports: []corev1.ContainerPort{
					{ContainerPort: 80, Protocol: corev1.ProtocolTCP},
				},
				ReadinessProbe: &corev1.Probe{
					ProbeHandler: corev1.ProbeHandler{
						HTTPGet: &corev1.HTTPGetAction{
							Path: "/",
							Port: intstr.FromInt32(80),
						},
					},
					PeriodSeconds: 2,
				},
			},
		},
	}
}

func deployFixtures(ctx context.Context) error {
	deployLabels := map[string]string{"app": "test-nginx-deploy"}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-nginx-pod",
			Namespace: testNS,
			Labels:    map[string]string{"app": "test-nginx-pod"},
		},
		Spec: nginxPodSpec(),
	}

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-nginx-svc",
			Namespace: testNS,
		},
		Spec: corev1.ServiceSpec{
			Selector: deployLabels,
			Ports: []corev1.ServicePort{
				{Name: "http", Port: 80, TargetPort: intstr.FromInt32(80), Protocol: corev1.ProtocolTCP},
			},
		},
	}

	replicas := int32(1)
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-nginx-deploy",
			Namespace: testNS,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: deployLabels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: deployLabels},
				Spec:       nginxPodSpec(),
			},
		},
	}

	if _, err := kubeClient.CoreV1().Pods(testNS).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create pod: %w", err)
	}
	if _, err := kubeClient.CoreV1().Services(testNS).Create(ctx, svc, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create service: %w", err)
	}
	if _, err := kubeClient.AppsV1().Deployments(testNS).Create(ctx, deploy, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create deployment: %w", err)
	}

	if err := waitForPodReady(ctx, testNS, "test-nginx-pod"); err != nil {
		return fmt.Errorf("wait for pod ready: %w", err)
	}
	if err := waitForDeploymentReady(ctx, testNS, "test-nginx-deploy"); err != nil {
		return fmt.Errorf("wait for deployment ready: %w", err)
	}

	createdSvc, err := kubeClient.CoreV1().Services(testNS).Get(ctx, "test-nginx-svc", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get service: %w", err)
	}
	svcClusterIP = createdSvc.Spec.ClusterIP
	fmt.Printf("Fixtures ready. Service ClusterIP: %s\n", svcClusterIP)
	return nil
}

func waitForPodReady(ctx context.Context, namespace, name string) error {
	for {
		pod, err := kubeClient.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		for _, cond := range pod.Status.Conditions {
			if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout waiting for pod %s/%s to be ready", namespace, name)
		case <-time.After(2 * time.Second):
		}
	}
}

func waitForDeploymentReady(ctx context.Context, namespace, name string) error {
	for {
		deploy, err := kubeClient.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if deploy.Status.ReadyReplicas >= 1 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout waiting for deployment %s/%s to be ready", namespace, name)
		case <-time.After(2 * time.Second):
		}
	}
}

func cleanupServerPods(ctx context.Context) {
	pods, err := kubeClient.CoreV1().Pods(metav1.NamespaceDefault).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/name=krelay-server",
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "list krelay-server pods: %v\n", err)
		return
	}
	for _, pod := range pods.Items {
		fmt.Printf("Cleaning up leftover server pod %s...\n", pod.Name)
		_ = kubeClient.CoreV1().Pods(metav1.NamespaceDefault).Delete(ctx, pod.Name, metav1.DeleteOptions{})
	}
}

// krelayInstance manages a running krelay process.
type krelayInstance struct {
	t       *testing.T
	cmd     *exec.Cmd
	mu      sync.Mutex
	output  []string
	stopped bool
}

// startKrelay launches a krelay process and waits until it logs the readyPattern.
func startKrelay(t *testing.T, readyPattern string, args ...string) *krelayInstance {
	t.Helper()

	fullArgs := append([]string{}, args...)
	if img := os.Getenv("KRELAY_SERVER_IMAGE"); img != "" {
		fullArgs = append(fullArgs, "--server.image", img)
	}

	cmd := exec.Command(krelayBin, fullArgs...)

	r, w, err := os.Pipe()
	require.NoError(t, err)
	cmd.Stdout = w
	cmd.Stderr = w

	require.NoError(t, cmd.Start())
	w.Close()

	ki := &krelayInstance{t: t, cmd: cmd}

	ready := make(chan struct{})
	var readyOnce sync.Once

	go func() {
		scanner := bufio.NewScanner(r)
		for scanner.Scan() {
			line := scanner.Text()
			ki.mu.Lock()
			ki.output = append(ki.output, line)
			ki.mu.Unlock()
			if strings.Contains(line, readyPattern) {
				readyOnce.Do(func() { close(ready) })
			}
		}
		r.Close()
	}()

	select {
	case <-ready:
		t.Log("krelay is ready")
	case <-time.After(3 * time.Minute):
		ki.stop()
		ki.mu.Lock()
		out := strings.Join(ki.output, "\n")
		ki.mu.Unlock()
		t.Fatalf("krelay did not become ready within 3 minutes. Output:\n%s", out)
	}

	t.Cleanup(ki.stop)
	return ki
}

func (ki *krelayInstance) stop() {
	ki.mu.Lock()
	if ki.stopped {
		ki.mu.Unlock()
		return
	}
	ki.stopped = true
	ki.mu.Unlock()

	_ = ki.cmd.Process.Signal(os.Interrupt)
	done := make(chan error, 1)
	go func() { done <- ki.cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		_ = ki.cmd.Process.Kill()
		<-done
	}
}

func (ki *krelayInstance) dumpOutput() string {
	ki.mu.Lock()
	defer ki.mu.Unlock()
	return strings.Join(ki.output, "\n")
}

// freePort returns an available TCP port on localhost.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port
}

// httpGetOK performs an HTTP GET and asserts a 200 response.
func httpGetOK(t *testing.T, url string) {
	t.Helper()
	client := &http.Client{Timeout: 10 * time.Second}
	var resp *http.Response
	var err error
	for range 10 {
		resp, err = client.Get(url)
		if err == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	require.NoError(t, err, "HTTP GET %s failed", url)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "unexpected status from %s", url)
}
