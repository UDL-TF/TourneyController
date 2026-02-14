package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
	"k8s.io/klog/v2"

	"github.com/UDL-TF/TourneyController/internal/chart"
	"github.com/UDL-TF/TourneyController/internal/config"
	"github.com/UDL-TF/TourneyController/internal/controller"
	"github.com/UDL-TF/TourneyController/internal/database"
)

func main() {
	klog.InitFlags(nil)
	defer klog.Flush()

	var kubeconfig string
	if home := homedir.HomeDir(); home != "" {
		kubeconfig = filepath.Join(home, ".kube", "config")
	}

	flag.StringVar(&kubeconfig, "kubeconfig", kubeconfig, "Path to the kubeconfig file. If running in-cluster, leave empty")
	flag.Parse()

	appCfg, err := config.Load()
	if err != nil {
		klog.Fatalf("failed to load controller config: %v", err)
	}

	restCfg, err := loadConfig(kubeconfig)
	if err != nil {
		klog.Fatalf("failed to load Kubernetes configuration: %v", err)
	}

	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		klog.Fatalf("failed to create Kubernetes clientset: %v", err)
	}

	repo, err := database.New(appCfg.Database)
	if err != nil {
		klog.Fatalf("failed to connect to postgres: %v", err)
	}
	defer repo.Close()

	renderer, err := chart.NewRenderer(restCfg, appCfg.Chart.Path, appCfg.Chart.ValuesFile, appCfg.Namespace)
	if err != nil {
		klog.Fatalf("failed to initialize chart renderer: %v", err)
	}

	ctrl := controller.New(appCfg, repo, clientset, renderer)

	ctx, cancel := signalContext()
	defer cancel()

	if err := ctrl.Run(ctx); err != nil && err != context.Canceled {
		klog.Fatalf("controller exited with error: %v", err)
	}
}

func loadConfig(kubeconfig string) (*rest.Config, error) {
	cfg, err := rest.InClusterConfig()
	if err == nil {
		return cfg, nil
	}

	return clientcmd.BuildConfigFromFlags("", kubeconfig)
}

func signalContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		stop := make(chan os.Signal, 1)
		signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
		<-stop
		klog.Info("received termination signal")
		cancel()
	}()

	return ctx, cancel
}
