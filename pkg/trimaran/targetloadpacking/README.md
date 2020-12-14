# Overview

This folder holds the `TargetLoadPacking` plugin implementation based on [Trimaran: Real Load Aware Scheduling](https://github.com/kubernetes-sigs/scheduler-plugins/blob/master/kep/61-Trimaran-real-load-aware-scheduling).

## Maturity Level

<!-- Check one of the values: Sample, Alpha, Beta, GA -->

- [ ] 💡 Sample (for demonstrating and inspiring purpose)
- [x] 👶 Alpha (used in companies for pilot projects)
- [ ] 👦 Beta (used in companies and developed actively)
- [ ] 👨 Stable (used in companies for production workloads)

## TargetLoadPacking Plugin
`TargetLoadPacking` depends on [Load Watcher](https://github.com/paypal/load-watcher) service.

If deploying from inside your cluster, make sure to comment out line 4 and 5 in `manifests/trimaran/scheduler-config.yaml`

You can configure `targetUtilization`, `defaultRequests` and `watcherAddress` in `TargetLoadPackingArgs`. Following is an example config:

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
      targetUtilization: 80 
      defaultRequests:
        cpu: "2000m"
      watcherAddress: http://127.0.0.1:2020
```

It is recommended to keep `targetUtilization` value 10 less than what you desire. `watcherAddress` is a required argument.