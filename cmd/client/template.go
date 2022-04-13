package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"text/template"
)

func getProgramName() string {
	if strings.HasPrefix(filepath.Base(os.Args[0]), "kubectl-") {
		return "kubectl relay"
	}
	return "krelay"
}

func example() string {
	const text = `
  # Listen on port 8080 locally, forwarding data to the port named "http" in the service
  {{.Name}} service/my-service 8080:http

  # Listen on a random port locally, forwarding udp packets to port 53 in a pod selected by the deployment
  {{.Name}} -n kube-system deploy/kube-dns :53@udp

  # Listen on port 5353 on all addresses, forwarding data to port 53 in the pod
  {{.Name}} --address 0.0.0.0 pod/my-pod 5353:53

  # Listen on port 6379 locally, forwarding data to "redis.cn-north-1.cache.amazonaws.com:6379" from the cluster
  {{.Name}} host/redis.cn-north-1.cache.amazonaws.com 6379

  # Listen on port 5000 and 6000 locally, forwarding data to "1.2.3.4:5000" and "1.2.3.4:6000" from the cluster
  {{.Name}} ip/1.2.3.4 5000@tcp 6000@udp

  # Create the agent in the kube-public namespace, and forward local port 5000 to "1.2.3.4:5000"
  {{.Name}} --server.namespace kube-public ip/1.2.3.4 5000
`
	tpl, err := template.New("example").Parse(text)
	if err != nil {
		panic(err)
	}
	var b bytes.Buffer
	_ = tpl.Execute(&b, map[string]string{
		"Name": getProgramName(),
	})
	return b.String()
}
