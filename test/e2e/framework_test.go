//go:build e2e

package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
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
	testRunID    string
	svcClusterIP string
)

func TestMain(m *testing.M) {
	var code int
	defer func() { os.Exit(code) }()

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

	fmt.Println("Building krelay binary...")
	krelayBin = filepath.Join(tmpDir, "krelay")
	buildCtx, buildCancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer buildCancel()
	buildCmd := exec.CommandContext(buildCtx, "go", "build", "-o", krelayBin, "./cmd/client")
	buildCmd.Dir = repoRoot
	buildCmd.Stdout = os.Stdout
	buildCmd.Stderr = os.Stderr
	if err := buildCmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "build krelay: %v\n", err)
		code = 1
		return
	}

	clusterCtx, clusterCancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer clusterCancel()

	kubeClient, err = buildKubeClient()
	if err != nil {
		fmt.Fprintf(os.Stderr, "create k8s client: %v\n", err)
		code = 1
		return
	}

	cleanupStaleNamespaces(clusterCtx)

	testRunID = fmt.Sprintf("%x", time.Now().UnixNano())
	testNS = fmt.Sprintf("krelay-e2e-%s", testRunID)
	fmt.Printf("Creating test namespace %s...\n", testNS)
	_, err = kubeClient.CoreV1().Namespaces().Create(clusterCtx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: testNS},
	}, metav1.CreateOptions{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "create namespace: %v\n", err)
		code = 1
		return
	}
	defer func() {
		fmt.Printf("Deleting test namespace %s...\n", testNS)
		_ = kubeClient.CoreV1().Namespaces().Delete(context.Background(), testNS, metav1.DeleteOptions{})
	}()

	fmt.Println("Deploying test fixtures...")
	if err := deployFixtures(clusterCtx); err != nil {
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

func cleanupStaleNamespaces(ctx context.Context) {
	nsList, err := kubeClient.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-30 * time.Minute)
	for _, ns := range nsList.Items {
		if strings.HasPrefix(ns.Name, "krelay-e2e-") && ns.CreationTimestamp.Time.Before(cutoff) {
			fmt.Printf("Cleaning up stale namespace %s...\n", ns.Name)
			_ = kubeClient.CoreV1().Namespaces().Delete(ctx, ns.Name, metav1.DeleteOptions{})
		}
	}
}

func serverPodPatch() string {
	patch := map[string]any{
		"metadata": map[string]any{
			"namespace": testNS,
			"labels": map[string]string{
				"krelay-e2e-run-id": testRunID,
			},
		},
	}
	b, _ := json.Marshal(patch)
	return string(b)
}

// krelayInstance manages a running krelay process.
type krelayInstance struct {
	t          *testing.T
	cmd        *exec.Cmd
	mu         sync.Mutex
	output     []string
	readyLines []string
	done       chan struct{}
	stopped    bool
}

var portRe = regexp.MustCompile(`(?:localAddr|address)=\S+:(\d+)`)

// startKrelay launches a krelay process and waits until readyCount occurrences
// of readyPattern appear in its output. It fails immediately if the process
// exits before becoming ready.
func startKrelay(t *testing.T, readyCount int, readyPattern string, args ...string) *krelayInstance {
	t.Helper()

	fullArgs := append([]string{}, args...)
	if img := os.Getenv("KRELAY_SERVER_IMAGE"); img != "" {
		fullArgs = append(fullArgs, "--server.image", img)
	}
	fullArgs = append(fullArgs, "--patch", serverPodPatch())

	cmd := exec.Command(krelayBin, fullArgs...)

	r, w, err := os.Pipe()
	require.NoError(t, err)
	cmd.Stdout = w
	cmd.Stderr = w

	require.NoError(t, cmd.Start())
	w.Close()

	ki := &krelayInstance{t: t, cmd: cmd, done: make(chan struct{})}

	go func() {
		_ = cmd.Wait()
		close(ki.done)
	}()

	ready := make(chan struct{})
	matchCount := 0

	go func() {
		scanner := bufio.NewScanner(r)
		for scanner.Scan() {
			line := scanner.Text()
			ki.mu.Lock()
			ki.output = append(ki.output, line)
			if strings.Contains(line, readyPattern) {
				ki.readyLines = append(ki.readyLines, line)
				matchCount++
				if matchCount >= readyCount {
					select {
					case <-ready:
					default:
						close(ready)
					}
				}
			}
			ki.mu.Unlock()
		}
		r.Close()
	}()

	select {
	case <-ready:
		t.Log("krelay is ready")
	case <-ki.done:
		ki.mu.Lock()
		out := strings.Join(ki.output, "\n")
		ki.mu.Unlock()
		t.Fatalf("krelay exited before becoming ready. Output:\n%s", out)
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
	select {
	case <-ki.done:
	case <-time.After(30 * time.Second):
		_ = ki.cmd.Process.Kill()
		<-ki.done
	}
}

func (ki *krelayInstance) dumpOutput() string {
	ki.mu.Lock()
	defer ki.mu.Unlock()
	return strings.Join(ki.output, "\n")
}

// localPorts extracts the bound local ports from the readiness log lines.
func (ki *krelayInstance) localPorts(t *testing.T) []int {
	t.Helper()
	ki.mu.Lock()
	defer ki.mu.Unlock()
	var ports []int
	for _, line := range ki.readyLines {
		m := portRe.FindStringSubmatch(line)
		if m != nil {
			p, _ := strconv.Atoi(m[1])
			ports = append(ports, p)
		}
	}
	require.NotEmpty(t, ports, "no local ports found in krelay output")
	return ports
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
