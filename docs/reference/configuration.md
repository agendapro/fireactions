# Configuration

Fireactions is configured using a YAML file. The default configuration file is located at `/etc/fireactions/config.yaml`.

You can also specify a custom configuration file using the `--config` flag when starting the Fireactions server.

Example configuration file with all available options:

```yaml
---
#
# The address to listen on for HTTP requests.
#
# Default: :8080
#
bind_address: 0.0.0.0:8080

#
# Metrics server configuration. This is used to expose Prometheus metrics on endpoint `/metrics`.
#
metrics:
  #
  # Enable Prometheus metrics.
  #
  enabled: true

  #
  # The address to listen on for HTTP requests.
  #
  address: 127.0.0.1:8081

#
# Runner cache configuration. This is used to run a bounded, disk-persistent
# cache component alongside the server, reachable from VMs over the CNI
# bridge. The cache serves public content only and has no credential
# configuration by design.
#
cache:
  #
  # Enable the runner cache.
  #
  # Default: false
  #
  enabled: false

  #
  # The address to listen on for HTTP requests. Must include a port and must
  # be an address reachable from the CNI bridge (e.g. the bridge gateway
  # 192.168.128.1:5050). There is no auto-detection.
  #
  # Required: true (when enabled)
  #
  bind_address: 192.168.128.1:5050

  #
  # The maximum amount of disk space the cache may use. Size suffixes follow
  # Docker's `--memory` convention: every suffix is interpreted as powers of 1024,
  # so `GB`, `GiB` and `G` are all synonyms (50GB == 50GiB == 50G == 53687091200
  # bytes). A bare number with no suffix is a raw byte count (max_size: 50 means
  # fifty bytes, not fifty gigabytes).
  #
  # Required: true (when enabled)
  #
  max_size: 50GB

  #
  # The directory where cache contents are stored on disk.
  #
  # Default: /var/lib/fireactions/cache
  #
  data_dir: /var/lib/fireactions/cache

#
# GitHub configuration.
#
github:
  #
  # The GitHub App private key. This is used to authenticate with GitHub.
  #
  # Default: ""
  #
  app_private_key: |
    -----BEGIN RSA PRIVATE KEY-----
  #
  # The GitHub App ID.
  #
  # Default: 0
  app_id: 12345

#
# Pools configuration.
#
pools:
  #
  # The name of the pool.
  #
- name: fireactions-2vcpu-2gb
  #
  # The number of replicas for the GitHub runners in the pool.
  #
  # Required: true
  #
  replicas: 5
  #
  # Shutdown the Firecracker VM when the runner exits.
  #
  # Required: false, Default: true
  #
  shutdown_on_exit: true
  #
  # GitHub runner configuration.
  #
  runner:
    #
    # The name of the GitHub runner. This is used to identify the runner in GitHub and is suffixed with a unique identifier.
    #
    # Required: true
    name: fireactions-2vcpu-2gb
    #
    # Container image to use for the Firecracker VM as the root device.
    #
    # Required: true
    #
    image: ghcr.io/hostinger/fireactions/runner:ubuntu-20.04-x64-2.310.2
    #
    # The pull policy for the container image. Can be one of: Always, IfNotPresent, Never.
    #
    # Required: true
    image_pull_policy: IfNotPresent
    #
    # GitHub runner group ID. 1 is the default group.
    #
    # Required: true
    group_id: 1
    #
    # Organization name.
    #
    # Required: true
    #
    organization: hostinger
    #
    # Labels to apply to the GitHub runner.
    #
    # Required: true
    #
    labels:
    - self-hosted
    - fireactions-2vcpu-2gb
    - fireactions
  #
  # Firecracker configuration.
  #
  firecracker:
    #
    # The path to the Firecracker binary.
    #
    # Default: firecracker
    #
    binary_path: firecracker
    #
    # The path to the kernel image.
    #
    # Required: true
    #
    kernel_image_path: /var/lib/fireactions/vmlinux
    #
    # Kernel command line arguments.
    #
    # Default: "console=ttyS0 noapic reboot=k panic=1 pci=off nomodules rw"
    #
    kernel_args: "console=ttyS0 noapic reboot=k panic=1 pci=off nomodules rw"
    #
    # Firecracker machine configuration.
    #
    # Required: true
    #
    machine_config:
      #
      # The amount of memory in MiB.
      #
      # Required: true
      #
      mem_size_mib: 2048
      #
      # The number of vCPUs.
      #
      # Required: true
      #
      vcpu_count: 2
    #
    # Metadata to pass to the Firecracker VM via MMDS.
    #
    # Default: {}
    #
    metadata:
      example1: value1
      example2: value2

#
# Log level. Can be one of: debug, info, warn, error, fatal, panic, trace.
#
# Default: info
#
log_level: debug
```
