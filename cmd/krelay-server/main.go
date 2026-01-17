package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"

	"github.com/knight/krelay/pkg/server"
)

func main() {
	var (
		addr     string
		certFile string
		keyFile  string
	)

	flag.StringVar(&addr, "addr", ":8443", "Address to listen on")
	flag.StringVar(&certFile, "tls-cert-file", "/etc/krelay/tls/tls.crt", "TLS certificate file")
	flag.StringVar(&keyFile, "tls-private-key-file", "/etc/krelay/tls/tls.key", "TLS private key file")

	klog.InitFlags(nil)
	flag.Parse()

	// Create Kubernetes client
	config, err := rest.InClusterConfig()
	if err != nil {
		klog.Fatalf("Failed to get in-cluster config: %v", err)
	}

	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		klog.Fatalf("Failed to create Kubernetes client: %v", err)
	}

	// Create handler
	handler := server.NewHandler(client)

	// Load TLS certificates
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		klog.Fatalf("Failed to load TLS certificates: %v", err)
	}

	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}

	// Create HTTP server
	srv := &http.Server{
		Addr:         addr,
		Handler:      handler,
		TLSConfig:    tlsConfig,
		ReadTimeout:  0, // No timeout for WebSocket
		WriteTimeout: 0,
	}

	// Start server
	go func() {
		klog.Infof("Starting krelay-server on %s", addr)
		if err := srv.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
			klog.Fatalf("Server error: %v", err)
		}
	}()

	// Wait for shutdown signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	klog.Info("Shutting down server...")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		klog.Errorf("Server shutdown error: %v", err)
	}

	fmt.Println("Server stopped")
}
