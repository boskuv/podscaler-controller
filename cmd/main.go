package main

import (
	"flag"
	"log"

	"k8s.io/client-go/tools/clientcmd"
)

func main() {
	// define a string flag for the kubeconfig file
	kubeconfig := flag.String("kubeconfig", "~/.kube/config", "absolute path to the kubeconfig file")

	flag.Parse()

	config, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		log.Fatalf("error building kubeconfig: %s", err.Error())
	}

	clientset, metricsClient := initClients(config)
	startScalingLoop(clientset, metricsClient)
}
