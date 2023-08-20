package remoteaddr

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	watchtools "k8s.io/client-go/tools/watch"
	"k8s.io/klog/v2"

	"github.com/knight42/krelay/pkg/xnet"
)

type dynamicAddr struct {
	podCli   typedcorev1.PodInterface
	selector string

	mu      sync.RWMutex
	podName string
	addr    xnet.Addr
}

var _ Getter = (*dynamicAddr)(nil)

func (d *dynamicAddr) Get() (xnet.Addr, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.addr, nil
}

func (d *dynamicAddr) watchForUpdates(w watch.Interface) {
	defer w.Stop()
	for ev := range w.ResultChan() {
		klog.V(5).InfoS("Receive event", "event", ev)

		switch ev.Type {
		case watch.Bookmark, watch.Error:
			klog.V(4).InfoS("Ignore specific events", "type", ev.Type)
			continue
		default: // make linter happy
		}

		pod, ok := ev.Object.(*corev1.Pod)
		if !ok || pod.Name != d.podName {
			klog.V(4).InfoS("Ignore event from unrelated pod", "pod", pod.Name, "current", d.podName)
			continue
		}

		if ev.Type == watch.Modified && pod.DeletionTimestamp == nil && pod.Status.Phase == corev1.PodRunning {
			klog.V(4).InfoS("Ignore event since the pod is still running", "pod", pod.Name)
			continue
		}

		klog.V(4).InfoS("Try to update remote address", "current", d.podName)
		err := wait.PollUntilContextTimeout(context.TODO(), time.Second*2, time.Minute, true, func(ctx context.Context) (bool, error) {
			_, err := d.updatePodIP(ctx)
			if err == nil {
				return true, nil
			}
			klog.Warningf("Fail to update remote address: %v. Will retry.", err)
			return false, nil
		})
		if err != nil {
			klog.Errorf("Fail to update remote address within timeout")
		} else {
			klog.V(4).InfoS("Successfully update remote address", "current", d.podName)
		}
	}
}

func (d *dynamicAddr) updatePodIP(ctx context.Context) (rv string, err error) {
	podList, err := d.podCli.List(ctx, metav1.ListOptions{
		LabelSelector: d.selector,
	})
	if err != nil {
		return "", fmt.Errorf("list pods: %w", err)
	}

	pods := podList.Items
	sort.Slice(pods, func(i, j int) bool {
		return !pods[i].CreationTimestamp.Before(&pods[j].CreationTimestamp)
	})
	for _, pod := range pods {
		if pod.Status.Phase == corev1.PodRunning {
			d.mu.Lock()
			d.podName = pod.Name
			d.addr, _ = xnet.AddrFromIP(pod.Status.PodIP)
			d.mu.Unlock()
			return podList.ResourceVersion, nil
		}
	}

	return "", errors.New("no running pods found")
}

func (d *dynamicAddr) init() error {
	rv, err := d.updatePodIP(context.Background())
	if err != nil {
		return err
	}

	w, err := watchtools.NewRetryWatcher(rv, &cache.ListWatch{
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			return d.podCli.Watch(context.Background(), options)
		}},
	)
	if err != nil {
		return fmt.Errorf("watch pods: %w", err)
	}

	go d.watchForUpdates(w)

	return nil
}
