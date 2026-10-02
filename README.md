# NautilusLB

NautilusLB is an open-source Layer 4 (TCP) load balancer designed for high availability and scalability in Kubernetes environments. It intelligently distributes incoming TCP traffic across multiple backend servers based on Kubernetes service definitions and custom annotations.

[![Go Tests](https://github.com/cloudresty/nautiluslb/actions/workflows/ci.yaml/badge.svg)](https://github.com/cloudresty/nautiluslb/actions/workflows/ci.yaml)
[![GitHub Tag](https://img.shields.io/github/v/tag/cloudresty/nautiluslb?label=Version)](https://github.com/cloudresty/nautiluslb/tags)
[![License](https://img.shields.io/badge/License-MIT-blue.svg)](https://opensource.org/licenses/MIT)

&nbsp;

## Table of Contents

- [How NautilusLB Works](#how-nautiluslb-works)
- [Why NautilusLB](#why-nautiluslb)
- [Key Features](#key-features)
- [Configuration](#configuration)
- [Service Binding](#service-binding)
- [Kubernetes Service Examples](#kubernetes-service-examples)
- [Kubernetes RBAC](#kubernetes-rbac)
- [Deployment](#deployment)
- [Docker Deployment](#docker-deployment)
- [Upgrading to v1.0.1](#upgrading-to-v101)
- [Example Scenario](#example-scenario)
- [Monitoring](#monitoring)
- [Contributing](#contributing)

🔝 [back to top](#nautiluslb)

&nbsp;

## How NautilusLB Works

NautilusLB operates as a reverse proxy, sitting in front of (or at the edge of) your Kubernetes cluster and forwarding incoming TCP connections to Kubernetes Services. Every 30 seconds it lists the cluster's nodes and the Services in each configuration's namespaces, and rebuilds the backend pool of every configuration from the Services that are explicitly bound to it (see [Service Binding](#service-binding)).

Backends are not pods. For a `NodePort` or `LoadBalancer` Service, a backend is every node's `InternalIP` paired with the Service's NodePort; for a `ClusterIP` Service, it is the Service's ClusterIP and port. Kubernetes then routes the connection to a pod as usual.

When a client establishes a TCP connection to NautilusLB, the listener it connected to selects the configuration, and NautilusLB picks a healthy backend of that configuration in round-robin order and forwards the connection. Every backend is health-checked with a TCP connect every 10 seconds; unhealthy backends are taken out of rotation until they pass again.

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
- **Round-Robin Selection:** Each new connection goes to the next healthy backend of its configuration

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

- **Dynamic Service Discovery:** NautilusLB polls the Kubernetes API every 30 seconds and picks up Services that opt in with the `nautiluslb.cloudresty.io/enabled` and `nautiluslb.cloudresty.io/configurations` annotations. New, changed and removed Services and nodes are reflected without a restart.
- **Explicit Service Binding:** A Service only receives traffic for the configurations it names, from namespaces the configuration allows. A Service in another namespace, or one that names a different configuration, can never join a listener's pool.
- **Layer 4 Load Balancing:** TCP-level round-robin load balancing across healthy backends.
- **Health Checking:** Every backend is checked with a TCP connect every 10 seconds (fixed). Unhealthy backends leave the rotation until they pass again; a backend that refuses a client connection is taken out at once.
- **Strict Configuration:** A YAML configuration file (`config.yaml`) defines listeners, port names and namespaces. Unknown keys and invalid values are refused at startup, all errors at once.
- **NodePort Support:** Load balances to Services exposed via NodePort (or `LoadBalancer` with node ports), which suits on-premise deployments and environments without an external load balancer integration.

🔝 [back to top](#nautiluslb)

&nbsp;

## Configuration

NautilusLB reads its configuration from `config.yaml` in its working directory (`/nautiluslb/config.yaml` in the container image). A commented example lives at [`app/config.example.yaml`](app/config.example.yaml). Here's an example configuration:

```yaml
#
# NautilusLB Configuration
#

# General settings
settings:
  kubeconfigPath: "/nautiluslb/kubeconfig"  # Kubeconfig to use when running outside the cluster

# Backend configurations
configurations:
  - name: http_traffic_configuration
    listenerAddress: ":80"         # Listen on port 80 on all interfaces
    requestTimeout: 5              # Backend connect timeout in seconds (capped at 10)
    backendPortName: "http"        # Name of the Service port to forward to
    namespaces: ["ingress-nginx"]  # Namespaces searched for Services

  - name: https_traffic_configuration
    listenerAddress: ":443"
    requestTimeout: 5
    backendPortName: "https"
    namespaces: ["ingress-nginx"]

  - name: mongodb_internal_service
    listenerAddress: ":27017"  # Internal service: expose on a private address only (see Docker Deployment)
    requestTimeout: 10
    backendPortName: "mongodb"
    namespaces: ["development"]

  - name: rabbitmq_amqp_internal_service
    listenerAddress: ":5672"
    requestTimeout: 10
    backendPortName: "amqp"
    namespaces: ["development"]
```

🔝 [back to top](#nautiluslb)

&nbsp;

### Configuration Parameters

- **`settings.kubeconfigPath`:** (Optional) Path to a kubeconfig file, used when NautilusLB runs outside the cluster. In-cluster configuration is always tried first. If this is empty and NautilusLB is not in a cluster, `~/.kube/config` of the user running it is used.
- **`configurations`:** A list of backend configurations, each defining one listener and the Services behind it. At least one is required.
  - **`name`:** A unique name, referenced by the `nautiluslb.cloudresty.io/configurations` annotation of Services. Letters, digits, `.`, `_` and `-`, starting and ending with a letter or digit, at most 63 characters.
  - **`listenerAddress`:** Where NautilusLB listens for this configuration: `":port"` for all interfaces, or `"IP:port"` to bind one address. Two configurations may not use overlapping listeners (`":80"` conflicts with `"10.0.0.1:80"`).
  - **`backendPortName`:** The name of the Service port to forward traffic to.
  - **`namespaces`:** The namespaces searched for Services. **At least one is required.** Use `["*"]` to opt into cluster-wide discovery; `"*"` cannot be combined with other entries.
  - **`namespace`:** (Legacy) A single namespace. Still accepted and merged with `namespaces`.
  - **`requestTimeout`:** (Optional) How long, in seconds, to wait when connecting to one backend before trying the next. Defaults to 5 and is capped at 10. It never limits how long an established connection stays open: listeners commonly carry websockets, SSE, database and cache sessions that stay open for hours.

The configuration is parsed strictly: an unknown key (for example a misspelt `namespaces`) is an error, not a silently ignored line. Duplicate names, conflicting listeners and invalid values are refused too, and every problem is reported at once. NautilusLB exits with status 1 on an invalid configuration, and also when a listener cannot bind (for example, the port is already in use).

🔝 [back to top](#nautiluslb)

&nbsp;

### Connection Handling

- A connection that cannot reach a backend is retried on up to three distinct backends, then closed cleanly. A backend that refuses or times out is taken out of rotation at once and restored by its next successful health check (every 10 seconds). If every backend is marked unhealthy, all of them are tried rather than none.
- Established connections have no idle timeout. Dead peers are detected by TCP keepalive (about 60 seconds). Once one side half-closes, the other direction must make progress at least every 2 minutes, and any single write may stall for at most 2 minutes.
- A failure in one connection, including a panic, closes that connection only.

🔝 [back to top](#nautiluslb)

&nbsp;

## Service Binding

A Kubernetes Service is a backend of configuration `C` only when **all** of the following hold:

1. it has the annotation `nautiluslb.cloudresty.io/enabled: "true"`;
2. it has the annotation `nautiluslb.cloudresty.io/configurations` whose comma-separated list contains `C.name` (for example `"http_traffic_configuration,https_traffic_configuration"`);
3. it is in one of `C`'s `namespaces` (or `C` uses `["*"]`);
4. it has a port named `C.backendPortName`.

The traffic NautilusLB sends depends on the Service type:

| Service type | Backends | Notes |
|---|---|---|
| `NodePort`, `LoadBalancer` | every node's `InternalIP` : the port's `nodePort` | Ports without a node port (`allocateLoadBalancerNodePorts: false`) are skipped |
| `ClusterIP` | the Service's `clusterIP` : the port's `port` | Only reachable where ClusterIPs are routed (in the cluster or on a node). Headless Services (`clusterIP: None`) are skipped |

A Service that is enabled but names no known configuration is ignored, and a warning is logged once.

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
    nautiluslb.cloudresty.io/configurations: 'http_traffic_configuration,https_traffic_configuration'
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
    nautiluslb.cloudresty.io/configurations: 'mongodb_internal_service'
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
    nautiluslb.cloudresty.io/configurations: 'rabbitmq_amqp_internal_service'
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

## Kubernetes RBAC

NautilusLB only lists nodes (for their `InternalIP`s) and Services. Give it a dedicated identity with exactly that, rather than an admin kubeconfig:

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: nautiluslb
  namespace: nautiluslb
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: nautiluslb-discovery
rules:
  - apiGroups: [""]
    resources: ["nodes"]
    verbs: ["list"]
  - apiGroups: [""]
    resources: ["services"]
    verbs: ["list"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: nautiluslb-discovery
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: nautiluslb-discovery
subjects:
  - kind: ServiceAccount
    name: nautiluslb
    namespace: nautiluslb
```

If no configuration uses `namespaces: ["*"]`, the `services` rule can be narrowed further to a `Role` and `RoleBinding` in each allowed namespace; `nodes` are cluster-scoped and always need the `ClusterRole`. When NautilusLB runs outside the cluster, build its kubeconfig from a token for this ServiceAccount (for example with `kubectl create token nautiluslb -n nautiluslb`, or a long-lived ServiceAccount token Secret).
The end-to-end test runs NautilusLB with exactly these permissions ([`test/e2e/rbac.yaml`](test/e2e/rbac.yaml)).

🔝 [back to top](#nautiluslb)

&nbsp;

## Deployment

### Prerequisites

- A Kubernetes cluster and an identity with the [RBAC permissions](#kubernetes-rbac) above
- A kubeconfig file for that identity (if running outside the cluster)
- Network reachability from NautilusLB to the nodes' `InternalIP`s (NodePort Services) or to the ClusterIPs (ClusterIP Services)

### Steps

1. **Build or obtain NautilusLB:** Use the container image `cloudresty/nautiluslb:<version>` from Docker Hub, or build the binary from source with `make build-local` (pre-built binaries are not published).
2. **Create configuration file:** Copy [`app/config.example.yaml`](app/config.example.yaml) to `config.yaml` and adapt it to your environment.
3. **Annotate your Services:** Add both annotations described in [Service Binding](#service-binding) to every Service NautilusLB should forward to.
4. **Run NautilusLB:** Start it from the directory that holds `config.yaml`. If running outside the cluster, set `kubeconfigPath` in `config.yaml`.

Example command:

```bash
./nautiluslb
```

🔝 [back to top](#nautiluslb)

&nbsp;

## Docker Deployment

The following example demonstrates how to run NautilusLB using a Docker container, with the configuration above:

```shell
docker run --detach \
  --name nautiluslb \
  --hostname nautiluslb \
  --volume /etc/cloudresty/nautiluslb/config.yaml:/nautiluslb/config.yaml:ro \
  --volume /etc/cloudresty/nautiluslb/kubeconfig:/nautiluslb/kubeconfig:ro \
  --restart unless-stopped \
  --publish 80:80 \
  --publish 443:443 \
  --publish 10.0.0.10:27017:27017 \
  --publish 10.0.0.10:5672:5672 \
  cloudresty/nautiluslb:v1.0.1
```

**Notes:**

- Use a specific version tag instead of `latest` for production deployments.
- Mount a dedicated, [least-privilege](#kubernetes-rbac) kubeconfig read-only and point `settings.kubeconfigPath` at it. Do not mount an administrator's `~/.kube/config`.
- The image runs as the non-root user `65532:65532` (distroless). Mounted files must be readable by that UID, for example `chown 65532:65532 kubeconfig && chmod 0400 kubeconfig`.
- Publish internal services (MongoDB, AMQP and the like) on a private host address only, as with `10.0.0.10:` above, never on all interfaces. When running the binary directly (or with `--network host`), set `listenerAddress` to `"10.0.0.10:27017"` instead.
- With the default bridge network Docker lets the non-root user bind ports 80 and 443. With `--network host` it cannot bind ports below 1024; keep bridge networking, or run with `--user 0` only if you accept running as root.

🔝 [back to top](#nautiluslb)

&nbsp;

## Upgrading to v1.0.1

v1.0.1 closes a traffic-hijacking hole: until v1.0.0, any Service anywhere in the cluster with `nautiluslb.cloudresty.io/enabled: "true"` and a port of the right name joined a listener's pool, so a tenant able to create a Service could receive a share of public `:80`/`:443` traffic. The fix requires changes to existing deployments.

**1. Every Service must name its configurations.**

Before:

```yaml
metadata:
  annotations:
    nautiluslb.cloudresty.io/enabled: 'true'
```

After:

```yaml
metadata:
  annotations:
    nautiluslb.cloudresty.io/enabled: 'true'
    nautiluslb.cloudresty.io/configurations: 'http_traffic_configuration,https_traffic_configuration'
```

**2. Every configuration must list its namespaces.** Omitting `namespace` no longer means cluster-wide; it is an error.

Before:

```yaml
configurations:
  - name: http_traffic_configuration
    listenerAddress: ":80"
    backendPortName: "http"
```

After:

```yaml
configurations:
  - name: http_traffic_configuration
    listenerAddress: ":80"
    backendPortName: "http"
    namespaces: ["ingress-nginx"]  # or ["*"] to keep cluster-wide discovery deliberately
```

**3. The configuration is parsed strictly.** Unknown keys, duplicate names, conflicting listeners, invalid names and malformed `listenerAddress` values (anything other than `":port"` or `"IP:port"`) now stop startup with exit status 1. A config that relied on a typo being ignored will now fail; fix the reported keys.

**4. `ClusterIP` Services are dialled on their `port`** (previously, and incorrectly, the `targetPort`). Headless Services and ports without a NodePort are skipped.

**5. The container image runs as non-root (UID 65532) and has no shell.** The binary and working directory are unchanged (`/nautiluslb/nautiluslb`, `/nautiluslb/config.yaml`). A kubeconfig mounted at `/root/.kube/config` is no longer readable: mount it elsewhere (for example `/nautiluslb/kubeconfig`), set `settings.kubeconfigPath`, and make it readable by UID 65532. The binary is now the image `ENTRYPOINT`, so arguments such as `-help` can be passed directly.

🔝 [back to top](#nautiluslb)

&nbsp;

## Example Scenario

Consider a Kubernetes cluster with an Ingress Nginx controller that you want to load balance HTTP and HTTPS traffic to using NautilusLB.

When a client sends an HTTP request to NautilusLB on port 80, the following process occurs:

1. NautilusLB receives the connection on port 80
2. The system identifies the target backend configuration as `http_traffic_configuration` based on the listener port
3. The pool of `http_traffic_configuration` holds the Services in `ingress-nginx` that are enabled, name `http_traffic_configuration` in their `nautiluslb.cloudresty.io/configurations` annotation and have an `http` port; for a NodePort Service, each node's `InternalIP` and the NodePort is one backend
4. The next healthy backend is selected in round-robin order
5. The client's TCP connection is forwarded to that node's NodePort, and Kubernetes routes it on to an Ingress NGINX pod

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

Contributions are welcome! Please see the [CONTRIBUTING.md](CONTRIBUTING.md) file for guidelines. `make test` runs the unit tests, `make lint` and `make vuln` the static checks, and `make e2e` an end-to-end test on a local [kind](https://kind.sigs.k8s.io/) cluster (needs Docker, kind and kubectl).

🔝 [back to top](#nautiluslb)

&nbsp;

---

&nbsp;

An open source project brought to you by the [Cloudresty](https://cloudresty.com) team.

[Website](https://cloudresty.com) &nbsp;|&nbsp; [LinkedIn](https://www.linkedin.com/company/cloudresty) &nbsp;|&nbsp; [BlueSky](https://bsky.app/profile/cloudresty.com) &nbsp;|&nbsp; [GitHub](https://github.com/cloudresty) &nbsp;|&nbsp; [Docker Hub](https://hub.docker.com/r/cloudresty/)

&nbsp;
