# k8sstorage

A small go http service that reads all clusters from the kubeconfig and retrieves and displays the total **requested pvc storage** by storage class in each cluster.

This service does not use the Kubernetes bloated client libraries. The matrix also includes a per-class all-clusters total and an all-classes total for each cluster. Pvcs without a `storageClassName` are grouped under `(no storage class)`. A cluster that cannot be queried is marked unavailable; its values are excluded from totals.

Hover over any cell to see more information, the tooltip for the row label `(no storage class)` shows each cluster's default storage class.
Other tooltips shows all pvc names and requested sizes included in that cell. Aggregate cells list the pvcs contributing to their totals.

## Run

```sh
KUBECONFIG=~/.kube/config ADDR=127.0.0.1:8080 go run .
```

Open <http://127.0.0.1:8080/>. Configuration can also be supplied with `-kubeconfig`, `-addr`, and `-exclude-clusters`. The equivalent environment variables are `KUBECONFIG`, `ADDR`, and `EXCLUDE_CLUSTERS`; the exclude option is a regular expression matched against cluster names. The listener defaults to loopback.

The service uses the current context's credentials for its cluster and the lexically first matching context for other clusters. It supports bearer tokens, token files, basic credentials, client certificates, certificate authorities, and `exec` credential plugins. Each context needs permission to list persistent volume claims across all namespaces and storage classes. The cluster header shows default classes identified by the stable annotation, or the legacy beta annotation when the stable annotation is absent. Classes available but unused in a cluster show a dash; cells for classes unavailable in that cluster are highlighted light red.

## Test

```sh
go test ./...
```
