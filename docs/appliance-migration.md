# Migrating to the appliance

This moves an existing akbench management cluster onto the [appliance](appliance.md)
**without recreating any workload cluster**. The source is the setup that
`main` documents: Cluster API, an infrastructure provider (Incus) and Flux installed by hand
on some cluster (minikube, kind, …), with the operator deployed via
`make deploy` or the chart, or run locally with `make dev`.

Workload clusters, the authentik deployments on them, their Postgres data and
their Prometheus history are untouched. Only the objects that *manage* them
change hands.

## What moves, and how

| Objects | How |
|---|---|
| `WorkloadCluster`s, and everything they own: the CAPI `Cluster`/`LXCCluster`/`KubeadmControlPlane`/`MachineDeployment` tree, machines, CA and kubeconfig Secrets, and the CNI/CSI/observability `HelmRelease`s | `clusterctl move`. The `WorkloadCluster` CRD carries `clusterctl.cluster.x-k8s.io/move-hierarchy=true`, so clusterctl moves it with its whole owner tree and rebuilds the ownerReferences. Status is recomputed by the controllers. |
| `TestCaseTemplate`s, `TenantPreset`s, `AuthentikDeployment`s, `TestCase`s, `TestRun`s | Copied by the script, **including status**, so finished `TestRun`s stay finished and keep the time ranges the Compare view uses. |
| Hand-made Secrets and ConfigMaps in those namespaces (infrastructure credentials such as `lxc-secret`, `bench-fixtures-*`, external Postgres credentials, extra values, custom k6 scripts) | Copied by the script. Secrets and ConfigMaps that already exist on the appliance are kept. A copied Incus connection keeps the certificate the server already trusts, and shows up on the web UI's Infrastructure page as soon as a cluster uses it. |
| Infrastructure providers | The script enables, on the appliance, every infrastructure provider the source has installed, the same way the Infrastructure page's **Enable** button does, and waits until they're installed. |
| Operator-generated Secrets, the `HelmRelease`s of `AuthentikDeployment`s | Not copied. The appliance's operator regenerates them. helm-controller finds each existing helm release on its cluster and upgrades it in place, so nothing is reinstalled. |

**What does not carry over:** any `AuthentikDeployment` that runs on the
**old management cluster itself** (no `clusterRef`). The appliance does not
allow those, so the script stops until they're deleted or recreated on a
workload cluster. Metrics stored in the old cluster's Prometheus stay
behind.

## Before you start

1. **Run the appliance** with an akbench version that includes this
   migration support (the `WorkloadCluster` CRD has the move-hierarchy label),
   and wait for the `akbench … is ready` log line.
   - Use the same `AKBENCH_NAMESPACE` the old setup used (default
     `default`). `clusterctl move` can't rename namespaces.
2. **Get a kubeconfig context for the appliance** on the machine running the
   script:
   ```sh
   docker exec akbench cat /etc/rancher/k3s/k3s.yaml \
     | sed 's/: default$/: akbench/' > ~/.kube/akbench.yaml
   export KUBECONFIG=~/.kube/config:~/.kube/akbench.yaml
   ```
   The `sed` renames k3s's default context/cluster/user to `akbench`, so it
   doesn't collide with anything else in your kubeconfig.
3. **Check reachability**: the Docker host must reach your infrastructure's API
   (e.g. Incus) and every workload cluster's API load balancer, as the old
   management cluster did.
   The script checks the load balancers from inside the appliance
   (`--skip-netcheck` turns this off).
4. **Install the tools** on the machine running the script: `kubectl`, `jq`, and `clusterctl` at least
   as new as the source's Cluster API version.
5. **Check the providers**: the appliance's Cluster API and infrastructure provider versions must be **≥ the
   source's**. `clusterctl move` refuses otherwise, and the script's
   dry run catches it before changing anything. If the source is ahead,
   upgrade the appliance image first.
6. **Wait for provisioning**: every workload cluster must be fully provisioned. `clusterctl move`
   refuses to move a cluster that is still coming up.
7. **Stop any locally running operator** (`make dev` / `make run`). The script
   can only scale down Deployments it finds on the source cluster.

## Running it

```sh
hack/migrate-to-appliance.sh --from-context minikube --to-context akbench
```

It runs preflight checks (both clusters reachable, target ready, network,
`clusterctl move --dry-run`), prints its plan and asks before changing anything. Then
it:

0. **Installs missing infrastructure providers** on the appliance, if the
   source has any the appliance doesn't yet, and runs the dry run once
   they're in.
1. **Quiesces the source**: scales the akbench operator, apiserver and Flux
   helm-controller to 0. This step is required, not a precaution:
   `clusterctl move` deletes source objects *before* it strips their
   finalizers. If the old operator were still running, it would tear down the
   workload cluster a `WorkloadCluster` names. If the old helm-controller were
   still running, it would `helm uninstall` every release a moved
   `HelmRelease` points at.
2. **Holds the appliance's operator**: suspends its `HelmRelease` and scales
   it to 0, so it can't race `clusterctl move` by creating a fresh `Cluster`
   for a `WorkloadCluster` that arrives before its moved `Cluster` does.
3. **Copies** the hand-made Secrets and ConfigMaps.
4. Runs **`clusterctl move`** for each namespace.
5. **Copies** the remaining akbench objects with their status.
6. **Resumes** the appliance's operator and waits for every `WorkloadCluster`
   to report `Ready`.

Every step converges when re-run, so if anything fails partway, fix the
cause and run the script again.

## Afterwards

- In the web UI, check that every workload cluster is Ready, the kubectl
  shell lists every cluster's context, and the Grafana proxy works.
- Check the Infrastructure page: the migrated Incus connection is listed under
  the cluster that uses it, and **Test connection** reports it as reachable
  and trusted. Saving it there once makes it a UI-managed connection, for
  example to pin the server certificate.
- **Never scale the old operator or helm-controller back up.** Both management
  clusters would then fight over the same workload clusters.
- Decommission the old cluster. Either delete it outright (for example `minikube delete`), or
  first remove the leftover objects without triggering any cleanup on the
  workload clusters:
  ```sh
  hack/migrate-to-appliance.sh --from-context minikube --cleanup-source
  ```

## Rolling back

- **Before step 4 (the move):** scale the source Deployments back up. The
  script prints each original replica count. Then delete whatever the script
  copied onto the appliance.
- **After the move:** move back the same way, in reverse:
  1. Scale the appliance's operator to 0. Also scale its `flux-system/helm-controller`
     to 0, for the same reason the script stops the source's.
  2. Run `clusterctl move --to-kubeconfig-context minikube -n <ns>` from the
     appliance's context.
  3. Scale the source Deployments back up.
