# Overview

This folder holds the `TargetLoadPacking` plugin implementation based on [Trimaran: Real Load Aware Scheduling](https://github.com/kubernetes-sigs/scheduler-plugins/blob/master/kep/61-Trimaran-real-load-aware-scheduling).

## Maturity Level

<!-- Check one of the values: Sample, Alpha, Beta, GA -->

- [ ] 💡 Sample (for demonstrating and inspiring purpose)
- [x] 👶 Alpha (used in companies for pilot projects)
- [ ] 👦 Beta (used in companies and developed actively)
- [ ] 👨 Stable (used in companies for production workloads)

## Tutorial
This tutorial will guide you to start kube-scheduler with `TargetLoadPacking` plugin configured. It should work on any Kubernetes setup you have (cluster, kind, minikube etc.).
`TargetLoadPacking` depends on [Load Watcher](https://github.com/paypal/load-watcher) service.

First build Trimaran scheduler with the following command in `main.go` file and save the built binary as `kube-scheduler`:

```go
command := app.NewSchedulerCommand(
		app.WithPlugin(targetloadpacking.Name, targetloadpacking.New),
	)
```

The `trimaran` container image can be built with the following Docker file in root folder:

```dockerfile
FROM alpine:latest
ADD ./kube-scheduler /usr/local/bin/kube-scheduler
ADD ./manifests/trimaran/scheduler-config.yaml /home/scheduler-config.yaml
```
If deploying from inside your cluster, make sure to comment out line 4 and 5 in `manifests/trimaran/scheduler-config.yaml`

Instructions to build load-watcher container Docker image can be found [here](https://github.com/paypal/load-watcher).

Following is a deployment spec for Trimaran scheduler. Add Docker image locations for respective containers built. 

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: trimaran
  namespace: kube-system
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: trimaran-as-kube-scheduler
subjects:
- kind: ServiceAccount
  name: trimaran
  namespace: kube-system
roleRef:
  kind: ClusterRole
  name: system:kube-scheduler
  apiGroup: rbac.authorization.k8s.io
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: trimaran-as-volume-scheduler
subjects:
- kind: ServiceAccount
  name: trimaran
  namespace: kube-system
roleRef:
  kind: ClusterRole
  name: system:volume-scheduler
  apiGroup: rbac.authorization.k8s.io
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: trimaran-extension-apiserver
  namespace: kube-system
subjects:
- kind: ServiceAccount
  name: trimaran
  namespace: kube-system
roleRef:
  kind: Role
  name: extension-apiserver-authentication-reader
  apiGroup: rbac.authorization.k8s.io
---
apiVersion: apps/v1
kind: Deployment
metadata:
  labels:
    component: scheduler
    tier: control-plane
  name: trimaran
  namespace: kube-system
spec:
  selector:
    matchLabels:
      component: scheduler
      tier: control-plane
  replicas: 1
  template:
    metadata:
      labels:
        component: scheduler
        tier: control-plane
        version: second
    spec:
      serviceAccountName: trimaran
      containers:
      - command:
        - /usr/local/bin/kube-scheduler
        - --address=0.0.0.0
        - --leader-elect=false
        - --scheduler-name=trimaran
        - --config=/home/scheduler-config.yaml
        - -v=4
        image: # Add image location
        imagePullPolicy: Always
        name: trimaran
        resources:
          requests:
            cpu: '0.1'
        securityContext:
          privileged: false
      - command:
        - /usr/local/bin/load-watcher
        image: # Add image location
        imagePullPolicy: Always
        name: load-watcher
      hostNetwork: false
      hostPID: false

```

You can configure `TargetCPUUtilization`, `DefaultCPURequests` and `WatcherAddress` in `TargetLoadPackingArgs`. Following is an example config:

```yaml
apiVersion: kubescheduler.config.k8s.io/v1beta1
kind: KubeSchedulerConfiguration
leaderElection:
  leaderElect: false
profiles:
  - schedulerName: trimaran
    plugins:
      score:
        disabled:
          - name: NodeResourcesBalancedAllocation
          - name: NodeResourcesLeastAllocated
        enabled:
          - name: TargetLoadPacking
    pluginConfig:
      - name: TargetLoadPacking
        args:
          targetCPUUtilization: 80 
          defaultCPURequests:
            cpu: "2000m"
          watcherAddress: http://127.0.0.1:2020
```