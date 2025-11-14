package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"text/template"
)

func getProgramName() string {
	name := filepath.Base(os.Args[0])
	if strings.HasPrefix(name, "kubectl-") {
		return "kubectl relay"
	}
	return name
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

  # Customize the server, and forward local port 5000 to "1.2.3.4:5000"
  {{.Name}} --patch '{"metadata":{"namespace":"kube-public"},"spec":{"nodeSelector":{"k": "v"}}}' ip/1.2.3.4 5000

  # Forward traffic to multiple targets
  cat <<EOF | {{.Name}} -f -
-l 192.168.1.100 ip/1.2.3.4 5000
svc/my-service 8080:80
-n kube-system deploy/coredns 5353:53@udp
EOF
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
