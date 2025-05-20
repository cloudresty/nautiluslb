# NautilusLB

&nbsp;

**THIS PROJECT IS CURRENTLY IN ALPHA AND IS NOT RECOMMENDED FOR PRODUCTION ENVIRONMENTS. USE WITH CAUTION.**

&nbsp;

---

&nbsp;

NautilusLB is an open-source Layer 4 (TCP) load balancer designed for high availability and scalability in Kubernetes environments. It intelligently distributes incoming TCP traffic across multiple backend servers based on Kubernetes service definitions and custom annotations.

&nbsp;

## How NautilusLB Works

NautilusLB operates as a reverse proxy, sitting in front of your Kubernetes cluster and forwarding incoming TCP connections to the appropriate backend services. It dynamically discovers Kubernetes services that are marked for load balancing using a specific annotation and maintains a real-time view of their available endpoints.

When a client establishes a TCP connection to NautilusLB, the load balancer determines the target backend service based on the listener port (the port on which the client connected). It then selects a healthy backend server for that service and forwards the connection. NautilusLB continuously monitors the health of its backends and automatically removes unhealthy servers from the load balancing pool, ensuring high availability.

&nbsp;

## Key Features

* **Dynamic Service Discovery:** NautilusLB integrates with the Kubernetes API to automatically discover and track services annotated with `nautiluslb.cloudresty.io/enabled: "true"`. It adapts to changes in the cluster, such as new services, updated endpoints, or pod failures, without requiring manual configuration updates.
* **Layer 4 Load Balancing:** Provides efficient TCP-level load balancing, distributing client connections across healthy backend servers.
* **Health Checking:** Continuously monitors the health of backend servers using TCP connection checks and automatically removes unhealthy servers from the load balancing pool.
* **Configurable:** Uses a YAML configuration file (`config.yaml`) to define backend configurations, listener addresses, health check intervals, and other settings.
* **NodePort Support:** Can be used to load balance traffic to Kubernetes services exposed via NodePort, making it suitable for on-premise deployments or environments without external load balancer integrations.

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
  - name: nginx_ingress_http_configuration
    listenerAddress: ":80"  # Listen on port 80 for HTTP traffic
    requestTimeout: 5  # Timeout for backend requests (in seconds)
    backendLabelSelector: "app.kubernetes.io/name=ingress-nginx,app.kubernetes.io/component=controller"  # Select backend pods with this label
    backendPortName: "http"  # Name of the port in the backend service

  - name: nginx_ingress_https_configuration
    listenerAddress: ":443"
    backendLabelSelector: "app.kubernetes.io/name=ingress-nginx,app.kubernetes.io/component=controller"
    requestTimeout: 5
    backendPortName: "https"

  - name: mongodb_service_configuration
    listenerAddress: ":27017"
    backendLabelSelector: "app.kubernetes.io/component=mongos"
    requestTimeout: 10
    backendPortName: "mongodb"

  - name: rabbitmq_amqp_service_configuration
    listenerAddress: ":5672"
    backendLabelSelector: "app.kubernetes.io/component=rabbitmq"
    requestTimeout: 10
    backendPortName: "amqp"
```

&nbsp;

**Key Configuration Parameters:**

* **`settings.kubeconfigPath`:** (Optional) Path to your Kubernetes configuration file if NautilusLB is running outside the cluster. If empty, it will attempt to use the in-cluster configuration or the default kubeconfig file (`~/.kube/config`).
* **`configurations`:** A list of backend configurations, each defining how to handle traffic for a specific service.
  * **`name`:** A unique name for the backend configuration.
  * **`listenerAddress`:** The address on which NautilusLB will listen for incoming connections for this backend (e.g., `:80`, `:443`, `:27017`).
  * **`requestTimeout`:** (Optional) The timeout (in seconds) for requests forwarded to the backend servers.
  * **`backendLabelSelector`:** A Kubernetes label selector used to identify the backend pods for this service. NautilusLB will use this selector to discover the endpoints (IP addresses and ports) of the backend servers.
  * **`backendPortName`:** The name of the port in the backend service that corresponds to the listener address. This is used to determine which port to forward traffic to on the selected backend pods.

&nbsp;

### Kubernetes Service Examples

&nbsp;

NGiNX Ingress Service

```yaml
apiVersion: v1
kind: Service
metadata:
  name: ingress-nginx-controller
  namespace: ingress-nginx
  labels:
    ...
  annotations:
    nautiluslb.cloudresty.io/enabled: 'true'
    ...
...
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
    ...
  type: NodePort
...
```

&nbsp;

MongoDB Service Example

```yaml
apiVersion: v1
kind: Service
metadata:
  name: mongodb-service
  namespace: my-namespace
  labels:
    ...
  annotations:
    ...
    nautiluslb.cloudresty.io/enabled: 'true'
...
spec:
  ports:
    - name: mongodb
      protocol: TCP
      port: 27017
      targetPort: 27017
  selector:
    app.kubernetes.io/component: mongos
    ...
  type: NodePort
...
```

&nbsp;

RabbitMQ (AMQP) Service Example

```yaml
apiVersion: v1
kind: Service
metadata:
  name: rabbitmq-amqp-service
  namespace: my-namespace
  labels:
    ...
  annotations:
    ...
    nautiluslb.cloudresty.io/enabled: 'true'
...
spec:
  ports:
    - name: ampq
      protocol: TCP
      port: 5672
      targetPort: 5672
  selector:
    app.kubernetes.io/name: rabbitmq
    ...
  type: NodePort
...
```

&nbsp;

## Deployment

1. **Build or Obtain the NautilusLB Binary:** You can either build NautilusLB from source or download a pre-built binary (if available).
2. **Create `config.yaml`:** Create a configuration file tailored to your environment and the services you want to load balance.
3. **Run NautilusLB:** Execute the NautilusLB binary, providing the path to your `config.yaml` file as a command-line argument (if your application supports it) or ensuring it's in the same directory.  If running outside the cluster, make sure the `kubeconfigPath` in `config.yaml` is correctly set.

For example, if you have a pre-built binary named `nautiluslb` and your `config.yaml` is in the same directory, you can run it like this:

```bash
./nautiluslb
```

### Using Docker

Below example demonstrates how NautilusLB can run using a Docker container.

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

Please note that you should use a stable version of the container image instead of `latest`.

&nbsp;

## Example Scenario

Let's say you have a Kubernetes cluster with an Ingress Nginx controller running and you want to load balance HTTP and HTTPS traffic to it using NautilusLB. Your `config.yaml` might look like the example in the "Configuration" section.

When a client sends an HTTP request to NautilusLB on port 80, NautilusLB will:

1. Receive the connection on port 80.
2. Identify that the target backend configuration is `my_http_configuration` (based on the listener port).
3. Use the label selector `app.kubernetes.io/name=ingress-nginx,app.kubernetes.io/component=controller` to discover the endpoints of the Ingress Nginx controller pods.
4. Select a healthy Ingress Nginx pod (e.g., using a round-robin algorithm).
5. Forward the client's TCP connection to the selected Ingress Nginx pod.

The same process applies to HTTPS traffic on port 443, using the `my_https_configuration`.

&nbsp;

## Monitoring

To monitor NautilusLB, you can use standard logging tools to collect and analyze its log output. The logs provide information about:

* Incoming connections
* Backend selection
* Health check status
* Errors or warnings

&nbsp;

## Contributing

Contributions are welcome! Please see the [CONTRIBUTING.md](CONTRIBUTING.md) file for guidelines.

&nbsp;

---

Made with ♥️ by [Cloudresty](https://cloudresty.com).
