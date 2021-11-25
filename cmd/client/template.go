package main

import (
	"bytes"
	"text/template"
)

func example() string {
	const text = `
  # Listen on port 8080 locally, forwarding data to the port named "http" in the service
  {{.Name}} service/my-service 8080:http

  # Listen on a random port locally, forwarding udp packets to port 53 in a pod selected by the deployment
  {{.Name}} -n kube-system deploy/kube-dns :53@udp

  # Listen on port 6379 locally, forwarding data to host "redis.cn-north-1.cache.amazonaws.com"
  {{.Name}} host/redis.cn-north-1.cache.amazonaws.com 6379

  # Listen on port 5000 and 6000 locally, forwarding data to ip 1.2.3.4
  {{.Name}} ip/1.2.3.4 5000@tcp 6000@udp
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
