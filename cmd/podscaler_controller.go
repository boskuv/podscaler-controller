package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"gopkg.in/yaml.v2"
	v1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/metrics/pkg/client/clientset/versioned"
)

const (
	appLabel                       = "bt-server"
	cpuThresholdForScalingPerPod   = 40
	cpuThresholdForUnscalingPerPod = 10
	totalCPULimit                  = 32000
	maxCpuPerPod                   = 8000
	retryLimit                     = 3
	retryInterval                  = 1 * time.Minute
	taskInterval                   = 5 * time.Minute
)

// DeploymentConfig holds the configuration for each deployment
type DeploymentConfig struct {
	Namespace   string `yaml:"namespace"`
	AppLabel    string `yaml:"appLabel"`
	Priority    int    `yaml:"priority"`
	MaxCpuUsage int    `yaml:"maxCpuUsage"`
}

// Config holds the deployment configurations
type Config struct {
	Deployments []DeploymentConfig `yaml:"deployments"`
}

func loadConfig(filePath string) (*Config, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var config Config
	decoder := yaml.NewDecoder(file)
	err = decoder.Decode(&config)
	if err != nil {
		return nil, err
	}

	return &config, nil
}

func initClients(config *rest.Config) (*kubernetes.Clientset, *versioned.Clientset) {
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatalf("error creating kubernetes client: %s", err.Error())
	}

	metricsClient, err := versioned.NewForConfig(config)
	if err != nil {
		log.Fatalf("error creating metrics client: %s", err.Error())
	}

	return clientset, metricsClient
}

func startScalingLoop(clientset *kubernetes.Clientset, metricsClient *versioned.Clientset, config *Config) {

	for {

		for _, deploymentConfig := range config.Deployments {
			releasedReplicas := unscaleDeployments(clientset, metricsClient, deploymentConfig)

			// if releasedReplicas == 0 then there is no point in rescaling deployments
			if releasedReplicas != 0 {
				time.Sleep(retryInterval) // TODO: what for
				rescaleDeployments(releasedReplicas, clientset, deploymentConfig)
			}

			scaleDeployments(clientset, metricsClient, deploymentConfig) // deploymentConfig
		}

		time.Sleep(taskInterval) // TODO: gocron
	}
}

// unscaleDeployments ...
func unscaleDeployments(clientset *kubernetes.Clientset, metricsClient *versioned.Clientset, deploymentConfig DeploymentConfig) int32 {
	releasedReplicas := int32(0)
	deployments := listDeployments(clientset, deploymentConfig)

	for _, deployment := range deployments {

		if isMoreThanSingleDeployment(deployment) {

			labelSelector := labels.Set(deployment.Spec.Selector.MatchLabels).AsSelector()

			deploymentCpuUsage, err := calculateDeploymentCpuUsage(labelSelector, clientset, metricsClient)

			if err != nil {
				log.Printf("Error calculating CPU usage for deployment %s: %v", deployment.Name, err)
				continue
			}

			// TODO: comment
			if deploymentCpuUsage <= (cpuThresholdForUnscalingPerPod * int64(*deployment.Spec.Replicas)) {
				baseReplicasValue := *deployment.Spec.Replicas // TODO: -1
				err = updateDeploymentReplicasValue(deployment, 1, clientset)

				if err != nil {
					releasedReplicas += baseReplicasValue
				}

			}
		}
	}

	return releasedReplicas
}

// rescaleDeployments ...
func rescaleDeployments(releasedReplicas int32, clientset *kubernetes.Clientset, deploymentConfig DeploymentConfig) error {
	deployments := listDeployments(clientset, deploymentConfig)
	updatedReplicaFactor := recalculateReplicasValueUnscaled(releasedReplicas, deployments)

	if updatedReplicaFactor > 0 {

		for _, deployment := range deployments {
			if isMoreThanSingleDeployment(deployment) {
				updateDeploymentReplicasValue(deployment, updatedReplicaFactor, clientset)
			}
		}
	}

	return nil
}

// scaleDeployments ..
func scaleDeployments(clientset *kubernetes.Clientset, metricsClient *versioned.Clientset, deploymentConfig DeploymentConfig) {
	//unscaledDeployments, scaledDeployments, scaledReplicasCounter := categorizeDeployments(listDeployments(clientset))
	deploymentsToScale := []v1.Deployment{}
	scaledDeployments := []v1.Deployment{} // TODO: rename
	var scaledReplicasCounter int32
	//unscaledDeployments := []v1.Deployment{}

	deployments := listDeployments(clientset, deploymentConfig)

	for _, deployment := range deployments {
		switch isMoreThanSingleDeployment(deployment) {
		case false:
			labelSelector := labels.Set(deployment.Spec.Selector.MatchLabels).AsSelector()

			// list pods with the same labels as the deployment
			pods, err := clientset.CoreV1().Pods("default").List(context.TODO(), metav1.ListOptions{ // inject
				LabelSelector: labelSelector.String(),
			})

			if err != nil {
				// TODO
			}

			for _, pod := range pods.Items {

				// Считаем для отдельного pod, потому что находимся в области 'unscaledDeployments' (т.е. deployments с replicas == 1)
				cpuUsagePerPod, err := calculatePodCpuUsage(&pod, metricsClient)
				if err != nil {
					log.Printf("Error calculating CPU usage for pod %s: %v", pod.Name, err)
					continue
				}

				if cpuUsagePerPod > cpuThresholdForScalingPerPod {
					deploymentsToScale = append(deploymentsToScale, deployment)
					log.Printf("Found deployment to scale '%s'. Reason: cpuUsagePerPod > cpuThresholdForScalingPerPod (in millicores) [%v > %v]", deployment.Name, cpuUsagePerPod, cpuThresholdForScalingPerPod)
				}
			}

			// deploymentCpuUsage, err := calculatePodCpuUsage(labelSelector, clientset, metricsClient)

			if err != nil {
				log.Printf("Error calculating CPU usage for deployment %s: %v", deployment.Name, err)
				continue
			}
		case true:
			scaledDeployments = append(scaledDeployments, deployment)
			scaledReplicasCounter++
		}
	}

	if len(deploymentsToScale) > 0 {
		replicaFactor := calculateReplicasValue(deploymentsToScale, scaledReplicasCounter, clientset, metricsClient)

		for _, deployment := range deploymentsToScale {
			updateDeploymentReplicasValue(deployment, int32(replicaFactor), clientset)
		}
		for _, deployment := range scaledDeployments {
			updateDeploymentReplicasValue(deployment, int32(replicaFactor), clientset)
		}
	}
}

func listDeployments(clientset *kubernetes.Clientset, deploymentConfig DeploymentConfig) []v1.Deployment {
	deploymentsList, err := clientset.AppsV1().Deployments(deploymentConfig.Namespace).List(context.TODO(), metav1.ListOptions{
		LabelSelector: fmt.Sprintf("app=%s", deploymentConfig.AppLabel),
	})
	if err != nil {
		log.Fatalf("Error listing deployments: %s", err)
	}

	return deploymentsList.Items
}

// isMoreThanSingleDeployment checks if replicas value is more than 1. In this case we assume that deployment is probably scaled
// and have a chance to be unscaled
func isMoreThanSingleDeployment(deployment v1.Deployment) bool {
	return deployment.Spec.Replicas != nil && *deployment.Spec.Replicas > 1
}

// updateDeploymentReplicasValue updates the value of replicas for deployment
func updateDeploymentReplicasValue(deployment v1.Deployment, newReplicasValue int32, clientset *kubernetes.Clientset) error {
	deployment.Spec.Replicas = int32Ptr(newReplicasValue)
	var err error

	for attempt := 0; attempt < retryLimit; attempt++ {

		_, err = clientset.AppsV1().Deployments(deployment.Namespace).Update(context.TODO(), &deployment, metav1.UpdateOptions{})
		if err == nil {
			break
		}
		if attempt < retryLimit-1 {
			time.Sleep(retryInterval)
		} else {
			log.Printf("Failed to update deployment '%s': %v", deployment.Name, err)
		}
	}

	return err
}

// recalculateReplicasValueUnscaled ..
func recalculateReplicasValueUnscaled(releasedReplicas int32, deployments []v1.Deployment) int32 {
	scaledDeploymentsAmount := 0
	sumCurrentReplicas := int32(0)

	for _, deployment := range deployments {

		if isMoreThanSingleDeployment(deployment) {
			scaledDeploymentsAmount++
			sumCurrentReplicas += *deployment.Spec.Replicas
		}
	}

	if scaledDeploymentsAmount == 0 {
		return 0
	}

	return (sumCurrentReplicas + releasedReplicas) / int32(scaledDeploymentsAmount)
}

// TODO: rename!
func calculateReplicasValue(deploymentsToScale []v1.Deployment, scaledReplicasCounter int32, clientset *kubernetes.Clientset, metricsClient *versioned.Clientset) float32 {

	if scaledReplicasCounter == 0 {
		availableCPUs, err := calculateAvailableCPUs(clientset, metricsClient)
		if err != nil {
			log.Printf("...: %s", err)
		}
		log.Printf("replicasValue is calculated for all available CPUs because there are no already scaled deployments. Available CPUs (in millicores): %v; Amount of deployments to scale: %v", availableCPUs, len(deploymentsToScale))
		return float32(int(availableCPUs) / (len(deploymentsToScale) * maxCpuPerPod))
	}

	log.Printf("replicasValue is calculated by dividing used replicas between already scaled deployments")
	// TODO: fix uneffective way
	return float32(scaledReplicasCounter) / float32(len(deploymentsToScale)) // TODO: (len(scaledDeployments) + len(deploymentsToScale)))
}

// calculatePodCpuUsage ..
func calculatePodCpuUsage(pod *corev1.Pod, metricsClient *versioned.Clientset) (int64, error) {

	podMetrics, err := metricsClient.MetricsV1beta1().PodMetricses(pod.Namespace).Get(context.TODO(), pod.Name, metav1.GetOptions{})
	if err != nil {
		return 0, err // Return an error if metrics can't be fetched
	}

	cpuUsageTotal := int64(0) // Total CPU usage initialized to zero

	// Sum CPU usage for each container
	for _, container := range podMetrics.Containers {
		cpuUsage := container.Usage[corev1.ResourceCPU] // TODO: handle errors

		// Check what MilliValue() returns (millivalue example: 34086m = 34.086 cores)
		cpuUsageTotal += cpuUsage.MilliValue()
	}

	return cpuUsageTotal, nil
}

// calculateDeploymentCpuUsage calculates the CPU usage for a specific deployment.
func calculateDeploymentCpuUsage(labelSelector labels.Selector, clientset *kubernetes.Clientset, metricsClient *versioned.Clientset) (int64, error) {
	// List pods with the same labels as the deployment
	pods, err := clientset.CoreV1().Pods("default").List(context.TODO(), metav1.ListOptions{ // inject
		LabelSelector: labelSelector.String(),
	})
	if err != nil {
		return 0, err
	}

	cpuUsageTotal := int64(0)

	for _, pod := range pods.Items {

		podMetrics, err := metricsClient.MetricsV1beta1().PodMetricses(pod.Namespace).Get(context.TODO(), pod.Name, metav1.GetOptions{})
		if err != nil {
			continue
		}

		// Sum CPU usage for each pod
		for _, container := range podMetrics.Containers {
			cpuUsage := container.Usage[corev1.ResourceCPU] // TODO: handle errors

			// Check what MilliValue() returns (millivalue example: 34086m = 34.086 cores)
			cpuUsageTotal += cpuUsage.MilliValue()
		}
	}

	return cpuUsageTotal, nil
}

// calculateAvailableCPUs calculates the total available CPU resources in the Kubernetes cluster.
func calculateAvailableCPUs(clientset *kubernetes.Clientset, metricsClient *versioned.Clientset) (int64, error) {

	// Retrieve node metrics
	nodeMetricsList, err := metricsClient.MetricsV1beta1().NodeMetricses().List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		log.Fatalf("error listing node metrics: %s", err)
	}

	totalAvailableCPUs := int64(0) // Total CPU usage initialized to zero

	// Print CPU usage for each node in cores
	for _, nodeMetrics := range nodeMetricsList.Items {
		cpuUsagePerNode := nodeMetrics.Usage[corev1.ResourceCPU]

		// Retrieve the node details to get allocatable resources
		node, err := clientset.CoreV1().Nodes().Get(context.TODO(), nodeMetrics.Name, metav1.GetOptions{})
		if err != nil {
			return 0, fmt.Errorf("error getting node details: %w", err)
		}

		// Get allocatable CPU from node details
		cpuAllocatedPerNode := node.Status.Allocatable[corev1.ResourceCPU]

		// Calculate available CPU
		availableCPUs := cpuAllocatedPerNode.MilliValue() - cpuUsagePerNode.MilliValue() // Available CPU in millicores

		// Check what MilliValue() returns (millivalue example: 34086m = 34.086 cores)
		totalAvailableCPUs += availableCPUs

	}

	return totalAvailableCPUs, nil
}

// Utility function to return a pointer to an int32 value
func int32Ptr(i int32) *int32 {
	return &i
}
