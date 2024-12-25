# k8s podscaler-controller

This project implements a Kubernetes autoscaling service that monitors CPU usage for deployments and automatically scales their replicas based on specified thresholds. The service checks current CPU metrics to determine appropriate scaling actions.

## Features

- Monitors CPU usage of deployments in Kubernetes.
- Automatically scales deployments up or down based on CPU thresholds.
- Configurable CPU thresholds for scaling and unscaling.

## Requirements

- Kubernetes cluster (version >= 1.29)
- Go (version >= 1.21)
- Access to the Kubernetes API and Metrics Server

## Setup

### 1. Clone the Repository

```bash
git clone https://github.com/boskuv/podscaler-controller.git
cd podscaler-controller
```

### 2. Build the Project
Make sure you have Go installed and set up. Then, build the project:

```bash
cd cmd
go build -o podscaler main.go
```