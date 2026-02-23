package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
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

	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	command := os.Args[1]

	// Remove the command from args so flag parsing works correctly
	os.Args = append([]string{os.Args[0]}, os.Args[2:]...)

	var kubeconfig string
	if home := homedir.HomeDir(); home != "" {
		kubeconfig = filepath.Join(home, ".kube", "config")
	}

	flag.StringVar(&kubeconfig, "kubeconfig", kubeconfig, "Path to the kubeconfig file. If running in-cluster, leave empty")
	flag.Parse()

	switch command {
	case "run":
		runController(kubeconfig)
	case "delete":
		runDeleteCommand()
	default:
		fmt.Printf("Unknown command: %s\n\n", command)
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println("Usage:")
	fmt.Println("  controller run                        - Start the tournament controller")
	fmt.Println("  controller delete <match_id> <round_id> - Delete a tournament server")
	fmt.Println("")
	fmt.Println("Examples:")
	fmt.Println("  controller run")
	fmt.Println("  controller delete 123 456")
}

func runController(kubeconfig string) {
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

func runDeleteCommand() {
	args := flag.Args()
	if len(args) != 2 {
		fmt.Println("Error: delete command requires exactly 2 arguments: <match_id> <round_id>")
		fmt.Println("")
		printUsage()
		os.Exit(1)
	}

	matchID, err := strconv.Atoi(args[0])
	if err != nil {
		klog.Fatalf("Invalid match_id '%s': must be a number", args[0])
	}

	roundID, err := strconv.Atoi(args[1])
	if err != nil {
		klog.Fatalf("Invalid round_id '%s': must be a number", args[1])
	}

	// Load configuration
	appCfg, err := config.Load()
	if err != nil {
		klog.Fatalf("failed to load controller config: %v", err)
	}

	// Get kubeconfig from global flag
	var kubeconfig string
	if home := homedir.HomeDir(); home != "" {
		kubeconfig = filepath.Join(home, ".kube", "config")
	}
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "kubeconfig" {
			kubeconfig = f.Value.String()
		}
	})

	// Set up Kubernetes client
	restCfg, err := loadConfig(kubeconfig)
	if err != nil {
		klog.Fatalf("failed to load Kubernetes configuration: %v", err)
	}

	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		klog.Fatalf("failed to create Kubernetes clientset: %v", err)
	}

	// Set up database
	repo, err := database.New(appCfg.Database)
	if err != nil {
		klog.Fatalf("failed to connect to postgres: %v", err)
	}
	defer repo.Close()

	// Set up chart renderer
	renderer, err := chart.NewRenderer(restCfg, appCfg.Chart.Path, appCfg.Chart.ValuesFile, appCfg.Namespace)
	if err != nil {
		klog.Fatalf("failed to initialize chart renderer: %v", err)
	}

	// Create controller
	ctrl := controller.New(appCfg, repo, clientset, renderer)

	// Delete the server
	ctx := context.Background()
	if err := ctrl.DeleteServer(ctx, matchID, roundID); err != nil {
		klog.Fatalf("failed to delete server: %v", err)
	}

	fmt.Printf("Successfully deleted tournament server for match %d round %d\n", matchID, roundID)
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
