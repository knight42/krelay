package ports

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/knight42/krelay/pkg/constants"
)

func TestGetPortsFromPodSpec(t *testing.T) {
	testCases := map[string]struct {
		podSpec  corev1.PodSpec
		expected portsInObject
	}{
		"no ports": {
			podSpec: corev1.PodSpec{
				InitContainers: []corev1.Container{
					{
						Ports: []corev1.ContainerPort{
							{
								Name:          "test",
								ContainerPort: 123,
							},
						},
					},
				},
			},
			expected: portsInObject{
				Names:     map[string]portWithProtocol{},
				Protocols: map[uint16][]string{},
			},
		},
		"same port with different protocols": {
			podSpec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Ports: []corev1.ContainerPort{
							{
								Name:          "tcp-dns",
								ContainerPort: 53,
								Protocol:      corev1.ProtocolTCP,
							},
							{
								Name:          "test",
								ContainerPort: 80,
								Protocol:      corev1.ProtocolTCP,
							},
						},
					},
					{
						Ports: []corev1.ContainerPort{
							{
								Name:          "udp-dns",
								ContainerPort: 53,
								Protocol:      corev1.ProtocolUDP,
							},
						},
					},
				},
			},
			expected: portsInObject{
				Names: map[string]portWithProtocol{
					"tcp-dns": {
						Port:     53,
						Protocol: constants.ProtocolTCP,
					},
					"udp-dns": {
						Port:     53,
						Protocol: constants.ProtocolUDP,
					},
					"test": {
						Port:     80,
						Protocol: constants.ProtocolTCP,
					},
				},
				Protocols: map[uint16][]string{
					53: {constants.ProtocolTCP, constants.ProtocolUDP},
					80: {constants.ProtocolTCP},
				},
			},
		},
	}
	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			r := require.New(t)
			ports := getPortsFromPodSpec(&tc.podSpec)
			r.Equal(tc.expected, ports)
		})
	}
}

func TestGetPortsFromObject(t *testing.T) {
	testCases := map[string]struct {
		obj      runtime.Object
		expected portsInObject
	}{
		"service": {
			obj: &corev1.Service{
				Spec: corev1.ServiceSpec{
					Ports: []corev1.ServicePort{
						{
							Name:     "tcp-dns",
							Port:     53,
							Protocol: corev1.ProtocolTCP,
						},
						{
							Name:     "udp-dns",
							Port:     53,
							Protocol: corev1.ProtocolUDP,
						},
					},
				},
			},
			expected: portsInObject{
				Names: map[string]portWithProtocol{
					"tcp-dns": {
						Port:     53,
						Protocol: constants.ProtocolTCP,
					},
					"udp-dns": {
						Port:     53,
						Protocol: constants.ProtocolUDP,
					},
				},
				Protocols: map[uint16][]string{
					53: {constants.ProtocolTCP, constants.ProtocolUDP},
				},
			},
		},
		"deployment": {
			obj: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Ports: []corev1.ContainerPort{
										{
											Name:          "tcp-dns",
											ContainerPort: 53,
											Protocol:      corev1.ProtocolTCP,
										},
										{
											Name:          "udp-dns",
											ContainerPort: 53,
											Protocol:      corev1.ProtocolUDP,
										},
									},
								},
								{
									Ports: []corev1.ContainerPort{
										{
											Name:          "test",
											ContainerPort: 8080,
											Protocol:      corev1.ProtocolTCP,
										},
									},
								},
							},
						},
					},
				},
			},
			expected: portsInObject{
				Names: map[string]portWithProtocol{
					"tcp-dns": {
						Port:     53,
						Protocol: constants.ProtocolTCP,
					},
					"udp-dns": {
						Port:     53,
						Protocol: constants.ProtocolUDP,
					},
					"test": {
						Port:     8080,
						Protocol: constants.ProtocolTCP,
					},
				},
				Protocols: map[uint16][]string{
					53:   {constants.ProtocolTCP, constants.ProtocolUDP},
					8080: {constants.ProtocolTCP},
				},
			},
		},
	}
	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			r := require.New(t)
			got, err := getPortsFromObject(tc.obj)
			r.NoError(err)
			r.Equal(tc.expected, got)
		})
	}
}

func TestParsePorts(t *testing.T) {
	testCases := map[string]struct {
		obj         runtime.Object
		args        []string
		expected    []PortPair
		expectedErr error
	}{
		"simple": {
			args: []string{"5353@udp", ":8080", "8443:443@tcp"},
			expected: []PortPair{
				{
					LocalPort:  5353,
					RemotePort: 5353,
					Protocol:   constants.ProtocolUDP,
				},
				{
					LocalPort:  0,
					RemotePort: 8080,
					Protocol:   constants.ProtocolTCP,
				},
				{
					LocalPort:  8443,
					RemotePort: 443,
					Protocol:   constants.ProtocolTCP,
				},
			},
		},
		"port name as remote port": {
			args: []string{"udp-dns", ":tcp-dns", "5353:udp-dns"},
			obj: &corev1.Service{
				Spec: corev1.ServiceSpec{
					Ports: []corev1.ServicePort{
						{
							Name:     "udp-dns",
							Port:     53,
							Protocol: corev1.ProtocolUDP,
						},
						{
							Name:     "tcp-dns",
							Port:     53,
							Protocol: corev1.ProtocolTCP,
						},
					},
				},
			},
			expected: []PortPair{
				{
					LocalPort:  53,
					RemotePort: 53,
					Protocol:   constants.ProtocolUDP,
				},
				{
					LocalPort:  0,
					RemotePort: 53,
					Protocol:   constants.ProtocolTCP,
				},
				{
					LocalPort:  5353,
					RemotePort: 53,
					Protocol:   constants.ProtocolUDP,
				},
			},
		},
		"automatically determine protocol": {
			args: []string{"5353:53", "8080"},
			obj: &appsv1.StatefulSet{
				Spec: appsv1.StatefulSetSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Ports: []corev1.ContainerPort{
										{
											Name:          "tcp",
											ContainerPort: 8080,
											Protocol:      corev1.ProtocolTCP,
										},
										{
											Name:          "udp",
											ContainerPort: 53,
											Protocol:      corev1.ProtocolUDP,
										},
									},
								},
							},
						},
					},
				},
			},
			expected: []PortPair{
				{
					LocalPort:  5353,
					RemotePort: 53,
					Protocol:   constants.ProtocolUDP,
				},
				{
					LocalPort:  8080,
					RemotePort: 8080,
					Protocol:   constants.ProtocolTCP,
				},
			},
		},
		"override protocol": {
			args: []string{"5353@tcp"},
			obj: &appsv1.DaemonSet{
				Spec: appsv1.DaemonSetSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Ports: []corev1.ContainerPort{
										{
											Name:          "udp",
											ContainerPort: 53,
											Protocol:      corev1.ProtocolUDP,
										},
									},
								},
							},
						},
					},
				},
			},
			expected: []PortPair{
				{
					LocalPort:  5353,
					RemotePort: 5353,
					Protocol:   constants.ProtocolTCP,
				},
			},
		},
		"ambiguous protocol": {
			args: []string{"8080"},
			obj: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Ports: []corev1.ContainerPort{
								{
									Name:          "tcp",
									ContainerPort: 8080,
									Protocol:      corev1.ProtocolTCP,
								},
								{
									Name:          "udp",
									ContainerPort: 8080,
									Protocol:      corev1.ProtocolUDP,
								},
							},
						},
					},
				},
			},
			expectedErr: fmt.Errorf("ambiguous protocol of port: 8080: [tcp udp]"),
		},
		"port name not found": {
			args:        []string{"no-such-port"},
			obj:         &appsv1.Deployment{},
			expectedErr: fmt.Errorf(`port name not found: "no-such-port"`),
		},
		"unknown protocol": {
			args:        []string{"8080@sctp"},
			expectedErr: fmt.Errorf(`unknown protocol: "sctp"`),
		},
		"invalid port format": {
			args:        []string{"1:2:3"},
			expectedErr: fmt.Errorf(`invalid port format: "1:2:3"`),
		},
		"invalid remote port": {
			args:        []string{"foo"},
			expectedErr: fmt.Errorf(`invalid port: "foo"`),
		},
		"invalid local port": {
			args:        []string{"foo:123"},
			expectedErr: fmt.Errorf(`invalid port: "foo"`),
		},
	}
	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			r := require.New(t)
			p := NewParser(tc.args).WithObject(tc.obj)
			got, err := p.Parse()
			if tc.expectedErr != nil {
				r.Equal(tc.expectedErr, err)
				return
			}
			r.NoError(err)
			r.Equal(tc.expected, got)
		})
	}
}
