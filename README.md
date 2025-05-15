# NautilusLB

&nbsp;

**WARNING**

**This project is currently in alpha and is not recommended for production environments. Use with caution.**

&nbsp;

---

&nbsp;

NautilusLB is an open-source Layer 4 (TCP) load balancer designed for high availability and scalability.

&nbsp;

## Key Features

* **High Availability:** Ensures continuous service availability by distributing traffic across multiple backend servers.
* **Scalability:** Easily scale your application by adding or removing backend servers without interrupting service.
* **Health Checking:** Monitors the health of backend servers and automatically removes unhealthy servers from the load balancing pool.

&nbsp;

## Installation

```shell
docker pull cloudresty/nautiluslb:latest
```

&nbsp;

## Usage

Create a configuration file for NautilusLB called `config.yaml` with the following specifications:

```yaml
#
# NautilusLB Configuration
#

# General settings
settings:
  kubeconfig_path: ""

# Backend configurations
backendConfigurations:

  - name: my_http_configuration
    listener_address: "0.0.0.0:80"
    health_check_interval: 10
    label_selector: "app.kubernetes.io/name=ingress-nginx,app.kubernetes.io/component=controller"
    request_timeout: 5

  - name: my_https_configuration
    listener_address: "0.0.0.0:443"
    health_check_interval: 10
    label_selector: "app.kubernetes.io/name=ingress-nginx,app.kubernetes.io/component=controller"
    request_timeout: 5
```

Make sure the host machine has both `config.yaml` and `.kube/config` files and then start NautilusLB as a container:

```shell
docker run \
    --name nautiluslb \
    --hostname nautiluslb \
    --volume $(pwd)/nautiluslb/config.yaml:/nautiluslb/config.yaml \
    --volume $(HOME)/.kube/config:/root/.kube/config \
    --restart unless-stopped \
    nautiluslb:latest
```

---

Made with ♥️ by [Cloudresty](https://cloudresty.com)
