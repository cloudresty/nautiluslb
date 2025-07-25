# NautilusLB

NautilusLB is an open-source Layer 4 (TCP) load balancer designed for high availability and scalability in Kubernetes environments. It intelligently distributes incoming TCP traffic across multiple backend servers based on Kubernetes service definitions and custom annotations.

[![Go Reference](https://pkg.go.dev/badge/github.com/cloudresty/nautiluslb.svg)](https://pkg.go.dev/github.com/cloudresty/nautiluslb)
[![Go Tests](https://github.com/cloudresty/nautiluslb/actions/workflows/ci.yaml/badge.svg)](https://github.com/cloudresty/nautiluslb/actions/workflows/ci.yaml)
[![GitHub Tag](https://img.shields.io/github/v/tag/cloudresty/nautiluslb?label=Version)](https://github.com/cloudresty/nautiluslb/tags)
[![License](https://img.shields.io/badge/License-MIT-blue.svg)](https://opensource.org/licenses/MIT)

&nbsp;

## Table of Contents

- [How NautilusLB Works](#how-nautiluslb-works)
- [Why NautilusLB](#why-nautiluslb)
- [Key Features](#key-features)
- [Configuration](#configuration)
- [Kubernetes Service Examples](#kubernetes-service-examples)
- [Deployment](#deployment)
- [Docker Deployment](#docker-deployment)
- [Example Scenario](#example-scenario)
- [Monitoring](#monitoring)
- [Contributing](#contributing)

🔝 [back to top](#nautiluslb)

&nbsp;

## How NautilusLB Works

NautilusLB operates as a reverse proxy, sitting in front of your Kubernetes cluster and forwarding incoming TCP connections to the appropriate backend services. It dynamically discovers Kubernetes services that are marked for load balancing using a specific annotation and maintains a real-time view of their available endpoints.

When a client establishes a TCP connection to NautilusLB, the load balancer determines the target backend service based on the listener port (the port on which the client connected). It then selects a healthy backend server for that service and forwards the connection. NautilusLB continuously monitors the health of its backends and automatically removes unhealthy servers from the load balancing pool, ensuring high availability.

🔝 [back to top](#nautiluslb)

&nbsp;

## Why NautilusLB

NautilusLB takes a fundamentally different approach compared to traditional load balancing solutions, offering significant advantages in efficiency, security, and resource utilization.

🔝 [back to top](#nautiluslb)

&nbsp;

### Reverse Discovery Architecture

Unlike traditional cloud load balancers that require services to expose themselves externally and rely on cloud provider infrastructure, NautilusLB implements a **reverse discovery pattern**. Instead of services pushing their availability outward, NautilusLB actively discovers services from within the Kubernetes cluster and presents them externally. This approach offers several key advantages:

- **Resource Efficiency:** Eliminates the need for multiple cloud load balancer instances per service, reducing infrastructure costs and complexity
- **Centralized Management:** Single point of control for all load balancing decisions, simplifying configuration and monitoring
- **Reduced Network Overhead:** Direct communication with Kubernetes API eliminates intermediate service mesh or proxy layers
- **Lower Latency:** Fewer network hops between client requests and backend services

🔝 [back to top](#nautiluslb)

&nbsp;

### Security Advantages

- **Minimal Attack Surface:** Services remain internal to the cluster with only NautilusLB exposed externally
- **Network Isolation:** Backend services don't need external connectivity or public endpoints
- **Controlled Access:** Single entry point with centralized security policies and monitoring
- **No Cloud Dependencies:** Reduces exposure to cloud provider security vulnerabilities and misconfigurations

🔝 [back to top](#nautiluslb)

&nbsp;

### Operational Benefits

- **Cost Optimization:** Eliminates per-service load balancer costs common in cloud environments
- **Simplified Deployment:** No need for complex service mesh configurations or cloud-specific annotations
- **Vendor Independence:** Works across any Kubernetes environment without cloud provider lock-in
- **Unified Monitoring:** Single application to monitor instead of multiple cloud load balancer instances

🔝 [back to top](#nautiluslb)

&nbsp;

### Performance Characteristics

- **Direct TCP Proxying:** Layer 4 load balancing with minimal processing overhead
- **Efficient Health Checking:** Centralized health monitoring reduces redundant checks across multiple load balancers
- **Dynamic Scaling:** Automatically adapts to service changes without manual intervention
- **Connection Pooling:** Optimized connection handling for better resource utilization

🔝 [back to top](#nautiluslb)

&nbsp;

### Comparison to Traditional Solutions

| Aspect | Traditional Cloud LB | Service Mesh | NautilusLB |
|--------|---------------------|--------------|------------|
| **Resource Usage** | High (per-service) | High (sidecar per pod) | Low (single instance) |
| **Configuration** | Cloud-specific | Complex mesh config | Simple YAML |
| **Cost** | Pay per LB instance | Infrastructure overhead | Single deployment cost |
| **Security** | Multiple entry points | Complex policy mesh | Single controlled entry |
| **Vendor Lock-in** | High | Medium | None |
| **Operational Overhead** | Medium-High | High | Low |

This architecture makes NautilusLB particularly well-suited for organizations seeking cost-effective, secure, and efficient load balancing without the complexity and overhead of traditional solutions.

🔝 [back to top](#nautiluslb)

&nbsp;

## Key Features

- **Dynamic Service Discovery:** NautilusLB integrates with the Kubernetes API to automatically discover and track services annotated with `nautiluslb.cloudresty.io/enabled: "true"`. It adapts to changes in the cluster, such as new services, updated endpoints, or pod failures, without requiring manual configuration updates.
- **Layer 4 Load Balancing:** Provides efficient TCP-level load balancing, distributing client connections across healthy backend servers.
- **Health Checking:** Continuously monitors the health of backend servers using TCP connection checks and automatically removes unhealthy servers from the load balancing pool.
- **Namespace Support:** Supports namespace-aware service discovery, allowing targeted discovery of services within specific Kubernetes namespaces.
- **Configurable:** Uses a YAML configuration file (`config.yaml`) to define backend configurations, listener addresses, health check intervals, and other settings.
- **NodePort Support:** Can be used to load balance traffic to Kubernetes services exposed via NodePort, making it suitable for on-premise deployments or environments without external load balancer integrations.

🔝 [back to top](#nautiluslb)

&nbsp;

## Configuration

NautilusLB is configured using a YAML file named `config.yaml`. Here's an example configuration:

```yaml
#
# NautilusLB Configuration
#

# General settings
settings:
  kubeconfigPath: ""  # Path to your kubeconfig file (if running outside the cluster)

# Backend configurations
configurations:
  - name: http_traffic_configuration
    listenerAddress: ":80"  # Listen on port 80 for HTTP traffic
    requestTimeout: 5  # Timeout for backend requests (in seconds)
    backendPortName: "http"  # Name of the port in the backend service

  - name: https_traffic_configuration
    listenerAddress: ":443"
    requestTimeout: 5
    backendPortName: "https"

  - name: mongodb_internal_service
    listenerAddress: ":27017"
    requestTimeout: 10
    backendPortName: "mongodb"
    namespace: "development"  # Target specific namespace

  - name: rabbitmq_amqp_internal_service
    listenerAddress: ":15672"
    requestTimeout: 10
    backendPortName: "amqp"
    namespace: "development"  # Target specific namespace
```

🔝 [back to top](#nautiluslb)

&nbsp;

### Configuration Parameters

- **`settings.kubeconfigPath`:** (Optional) Path to your Kubernetes configuration file if NautilusLB is running outside the cluster. If empty, it will attempt to use the in-cluster configuration or the default kubeconfig file (`~/.kube/config`).
- **`configurations`:** A list of backend configurations, each defining how to handle traffic for a specific service.
  - **`name`:** A unique name for the backend configuration.
  - **`listenerAddress`:** The address on which NautilusLB will listen for incoming connections for this backend (e.g., `:80`, `:443`, `:27017`).
  - **`requestTimeout`:** (Optional) The timeout (in seconds) for requests forwarded to the backend servers.
  - **`namespace`:** (Optional) The Kubernetes namespace to discover services in. If omitted, services will be discovered across all namespaces.
  - **`backendPortName`:** The name of the port in the backend service that corresponds to the listener address. This is used to determine which port to forward traffic to on the selected backend pods.

🔝 [back to top](#nautiluslb)

&nbsp;

## Kubernetes Service Examples

### NGiNX Ingress Service

```yaml
apiVersion: v1
kind: Service
metadata:
  name: ingress-nginx-controller
  namespace: ingress-nginx
  labels:
    # ... other labels
  annotations:
    nautiluslb.cloudresty.io/enabled: 'true'
    # ... other annotations
spec:
  ports:
    - name: http
      protocol: TCP
      port: 80
      targetPort: 80
    - name: https
      protocol: TCP
      port: 443
      targetPort: 443
  selector:
    app.kubernetes.io/name: ingress-nginx
    app.kubernetes.io/component: controller
  type: NodePort
```

🔝 [back to top](#nautiluslb)

&nbsp;

### MongoDB Service Example

```yaml
apiVersion: v1
kind: Service
metadata:
  name: mongodb-service
  namespace: development
  labels:
    # ... other labels
  annotations:
    nautiluslb.cloudresty.io/enabled: 'true'
    # ... other annotations
spec:
  ports:
    - name: mongodb
      protocol: TCP
      port: 27017
      targetPort: 27017
  selector:
    app.kubernetes.io/component: mongos
  type: NodePort
```

🔝 [back to top](#nautiluslb)

&nbsp;

### RabbitMQ (AMQP) Service Example

```yaml
apiVersion: v1
kind: Service
metadata:
  name: rabbitmq-amqp-service
  namespace: development
  labels:
    # ... other labels
  annotations:
    nautiluslb.cloudresty.io/enabled: 'true'
    # ... other annotations
spec:
  ports:
    - name: amqp
      protocol: TCP
      port: 5672
      targetPort: 5672
  selector:
    app.kubernetes.io/name: rabbitmq
  type: NodePort
```

🔝 [back to top](#nautiluslb)

&nbsp;

## Deployment

### Prerequisites

- Kubernetes cluster with appropriate RBAC permissions for service discovery
- Access to kubeconfig file (if running outside the cluster)

### Steps

1. **Build or obtain the NautilusLB binary:** You can either build NautilusLB from source or download a pre-built binary.
2. **Create configuration file:** Create a `config.yaml` file tailored to your environment and the services you want to load balance.
3. **Run NautilusLB:** Execute the NautilusLB binary. If running outside the cluster, ensure the `kubeconfigPath` in `config.yaml` is correctly set.

Example command:

```bash
./nautiluslb
```

🔝 [back to top](#nautiluslb)

&nbsp;

## Docker Deployment

The following example demonstrates how to run NautilusLB using a Docker container:

```shell
docker run --detach \
  --name nautiluslb \
  --hostname nautiluslb \
  --volume /etc/cloudresty/nautiluslb/config.yaml:/nautiluslb/config.yaml \
  --volume /root/.kube/config:/root/.kube/config \
  --restart unless-stopped \
  --publish 80:80 \
  --publish 443:443 \
  --publish 5672:5672 \
  --publish 27017:27017 \
  cloudresty/nautiluslb:latest
```

**Note:** Use a specific version tag instead of `latest` for production deployments.

🔝 [back to top](#nautiluslb)

&nbsp;

## Example Scenario

Consider a Kubernetes cluster with an Ingress Nginx controller that you want to load balance HTTP and HTTPS traffic to using NautilusLB.

When a client sends an HTTP request to NautilusLB on port 80, the following process occurs:

1. NautilusLB receives the connection on port 80
2. The system identifies the target backend configuration as `http_traffic_configuration` based on the listener port
3. NautilusLB discovers services with the annotation `nautiluslb.cloudresty.io/enabled: "true"` that have an `http` port
4. A healthy backend endpoint is selected using the configured load balancing algorithm
5. The client's TCP connection is forwarded to the selected backend endpoint

The same process applies to HTTPS traffic on port 443, using the `https_traffic_configuration`.

🔝 [back to top](#nautiluslb)

&nbsp;

## Monitoring

NautilusLB provides comprehensive logging for monitoring and troubleshooting. The logs include information about:

- Incoming connections
- Backend selection
- Health check status
- Errors or warnings

You can use standard logging tools to collect and analyze the log output for operational insights.

🔝 [back to top](#nautiluslb)

&nbsp;

## Contributing

Contributions are welcome! Please see the [CONTRIBUTING.md](CONTRIBUTING.md) file for guidelines.

🔝 [back to top](#nautiluslb)

&nbsp;

---

&nbsp;

An open source project brought to you by the [Cloudresty](https://cloudresty.com) team.

[Website](https://cloudresty.com) &nbsp;|&nbsp; [LinkedIn](https://www.linkedin.com/company/cloudresty) &nbsp;|&nbsp; [BlueSky](https://bsky.app/profile/cloudresty.com) &nbsp;|&nbsp; [GitHub](https://github.com/cloudresty)

&nbsp;
