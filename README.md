# khelm ![GitHub workflow badge](https://github.com/mgoltzsche/khelm/workflows/Release/badge.svg) [![Go Report Card](https://goreportcard.com/badge/github.com/mgoltzsche/khelm)](https://goreportcard.com/report/github.com/mgoltzsche/khelm)

A [Helm](https://github.com/helm/helm) chart templating CLI, helm to kustomize converter, [kpt](https://github.com/GoogleContainerTools/kpt) function and [kustomize](https://github.com/kubernetes-sigs/kustomize/) plugin.  


## Motivation / History

[Helm](https://github.com/helm/helm) _charts_ provide a great way to share and reuse [Kubernetes](https://github.com/kubernetes/kubernetes) applications and there is a lot of them.
However writing helm templates is cumbersome and you cannot reuse a chart properly if it does not yet support a particular parameter/value.

[Kustomize](https://github.com/kubernetes-sigs/kustomize/) solves these issues declaratively by merging Kubernetes API objects which grants users of a _kustomization_ the freedom to change anything.
However kustomize neither supports lifecycle management nor templating with externally passed in values (which is sometimes still required).  

To overcome the gap between helm and kustomize initially this repository provided a kustomize plugin and [k8spkg](https://github.com/mgoltzsche/k8spkg) was used for lifecycle management.  
Since [kpt](https://github.com/GoogleContainerTools/kpt) is [published](https://opensource.googleblog.com/2020/03/kpt-packaging-up-your-kubernetes.html) helm and kustomize can be run as (chained) kpt functions supporting declarative, GitOps-based workflows. kpt also supports dynamic modification of static (rendered) manifests with externally passed in values using [setters](https://googlecontainertools.github.io/kpt/guides/consumer/set/) as well as [dependency](https://googlecontainertools.github.io/kpt/reference/pkg/) and [lifecycle management](https://googlecontainertools.github.io/kpt/reference/live/).


## Features

* Template/render a Helm chart
* Convert a chart's output into a kustomization
* Build local charts automatically when templating
* Use any repository without registering it in repositories.yaml
* Automatically fetch and updated required repository index files
* Enforce namespace-scoped resources within the template output
* Set a namespace on all resources

## Supported interfaces

khelm can be used as:
* [kpt function](#kpt-function) (recommended)
* [kustomize exec plugin](#kustomize-exec-plugin)
* [CLI](#cli)
* [Go API](#go-api)

Usage examples can be found in the [example](example) and [e2e](e2e) directories.

### kpt function

The khelm kpt function templates a chart and returns the output as single manifest file or kustomization directory (when `outputPath` ends with `/`). The kustomization output can be used to apply further transformations by running a kustomize function afterwards.  

Also, in opposite to the kustomize plugin approach, a kpt function does not depend on particular plugin binaries on the host and CD pipelines can run without dependencies to rendering technologies and chart servers since they just apply static mainfests (after changing values using `kpt cfg set`) located within a git repository to a cluster using `kpt live apply`.

#### kpt function usage example

A kpt function can be declared as annotated _ConfigMap_ within a kpt project.
A kpt project can be initialized and used with such a function as follows:
```sh
mkdir example-project && cd example-project
kpt pkg init . # Creates the Kptfile
cat - > khelm-function.yaml <<-EOF
  apiVersion: v1
  kind: ConfigMap
  metadata:
    name: cert-manager-manifest-generator
    annotations:
      config.kubernetes.io/function: |
        container:
          image: mgoltzsche/khelm:latest
          network: true
      config.kubernetes.io/local-config: "true"
  data:
    repository: https://charts.jetstack.io
    chart: cert-manager
    version: 0.9.x
    name: my-cert-manager-release
    namespace: cert-manager
    values:
      webhook:
        enabled: false
    outputPath: output-manifest.yaml
EOF
kpt fn run --network . # Renders the chart into output-manifest.yaml
```
_For all available fields see the [table](#configuration-options) below._  

Please note that, in case you need to refer to a local chart directory or values file, the source must be mounted to the function using `kpt fn run --mount=<SRC_MOUNT> .`.  
The [example kpt project](example/kpt) and the corresponding [e2e test](e2e/kpt-function-test.sh) show how to do that.  

Kpt can also be leveraged to pull charts from other git repositories into your own repository using the `kpt pkg sync .` [command](https://googlecontainertools.github.io/kpt/reference/pkg/) (with a corresponding dependency set up) before running the khelm function (for this reason the go-getter support has been removed from this project).  

If necessary the chart output can be transformed using kustomize.
This can be done by declaring the khelm and a kustomize function orderly within a file and specifying the chart output kustomization as input for the kustomize function as shown in [this example](example/kpt/helm-kustomize-pipeline.yaml).


### kustomize exec plugin

khelm can be used as [kustomize](https://github.com/kubernetes-sigs/kustomize) 3 [exec plugin](https://kubectl.docs.kubernetes.io/guides/extending_kustomize/execpluginguidedexample/).
Though plugin support in kustomize is still an alpha feature and may be removed in a future version.

#### Plugin installation

Install using curl (linux amd64):
```sh
mkdir -p $HOME/.config/kustomize/plugin/khelm.mgoltzsche.github.com/v2/chartrenderer
curl -fsSL https://github.com/mgoltzsche/khelm/releases/latest/download/khelm-linux-amd64 > $HOME/.config/kustomize/plugin/khelm.mgoltzsche.github.com/v2/chartrenderer/ChartRenderer
chmod u+x $HOME/.config/kustomize/plugin/khelm.mgoltzsche.github.com/v2/chartrenderer/ChartRenderer
```
or using `go`:
```sh
go get github.com/mgoltzsche/khelm/cmd/khelm
mkdir -p $HOME/.config/kustomize/plugin/khelm.mgoltzsche.github.com/v2/chartrenderer
mv $GOPATH/bin/khelm $HOME/.config/kustomize/plugin/khelm.mgoltzsche.github.com/v2/chartrenderer/ChartRenderer
```

#### Plugin usage example

A _plugin descriptor_ specifies the helm repository, chart, version and values that should be used in a kubernetes-style resource can be referenced in the `generators` section of a `kustomization.yaml` and can look as follows:
```yaml
apiVersion: khelm.mgoltzsche.github.com/v2
kind: ChartRenderer
metadata:
  name: cert-manager # fallback for `name`
  namespace: cert-manager # fallback for `namespace`
repository: https://charts.jetstack.io
chart: cert-manager
version: 0.9.x
values:
  webhook:
    enabled: false
```
_For all available fields see the [table](#configuration-options) below._

More complete examples can be found within the [example](example) directory.
For instance `cert-manager` can be rendered like this:
```sh
kustomize build --enable_alpha_plugins github.com/mgoltzsche/khelm/example/cert-manager
```

### CLI

khelm also supports a helm-like `template` CLI.

#### Binary installation
```sh
curl -fsSL https://github.com/mgoltzsche/khelm/releases/latest/download/khelm-linux-amd64 > khelm
chmod +x khelm
sudo mv khelm /usr/local/bin/khelm
```

#### Binary usage example
```sh
khelm template cert-manager --version=0.9.x --repo=https://charts.jetstack.io
```
_For all available options see the [table](#configuration-options) below._

#### Docker usage example
```sh
docker run mgoltzsche/khelm:latest template cert-manager --version=0.9.x --repo=https://charts.jetstack.io
```

### Go API

The khelm Go API `github.com/mgoltzsche/khelm/v2/pkg/helm`exposes a `Helm` struct that provides a `Render()` function that returns the rendered resources as `kyaml` objects.

## Configuration options

| Field | CLI        | Description |
| ----- | ---------- | ----------- |
| `chart` | ARGUMENT    | Chart file (if `repository` not set) or name. |
| `version` | `--version` | Chart version. Latest version is used if not specified. |
| `repository` | `--repo` | URL to the repository the chart should be loaded from. |
| `valueFiles` | `-f` | Locations of values files.
| `values` | `--set` | Set values object or in CLI `key1=val1,key2=val2`. |
| `apiVersions` | `--api-versions` | Kubernetes api versions used for Capabilities.APIVersions. |
| `name` | `--name` | Release name used to render the chart. |
| `verify` | `--verify` | If enabled verifies the signature of all charts using the `keyring` (see [Helm 3 provenance and integrity](https://helm.sh/docs/topics/provenance/)). |
| `keyring` | `--keyring` | GnuPG keyring file (default `~/.gnupg/pubring.gpg`). |
| `excludeCRDs` | `--skip-crds` | If true Custom Resource Definitions are excluded from the output. |
| `exclude` |  | List of resource selectors that exclude matching resources from the output. Fails if a selector doesn't match any resource. |
| `exclude[].apiVersion` |  | Excludes resources by apiVersion. |
| `exclude[].kind` |  | Excludes resources by kind. |
| `exclude[].namespace` |  | Excludes resources by namespace. |
| `exclude[].name` |  | Excludes resources by name. |
| `namespace` | `--namespace` | Set the namespace used by Helm templates. |
| `namespacedOnly` | `--namespaced-only` | If enabled fail on known cluster-scoped resources and those of unknown kinds. |
| `forceNamespace` | `--force-namespace` | Set namespace on all namespaced resources (and those of unknown kinds). |
| `output` | `--output` | Path to write the output to. If it ends with `/` a kustomization is generated. (Not supported by the kustomize plugin.) |
|  | `--output-replace` | If enabled replace the output directory or file (CLI-only). |
|  | `--trust-any-repo` | If enabled repositories that are not registered within `repositories.yaml` can be used as well (env var `KHELM_TRUST_ANY_REPO`). Within the kpt function this behaviour can be disabled by mounting `/helm/repository/repositories.yaml` or disabling network access. |
| `debug` | `--debug` | Enables debug log and provides a stack trace on error. |

### Repository configuration

Repository credentials can be configured using helm's `repositories.yaml` which can be passed through as `Secret` to generic build jobs. khelm downloads repo index files when needed.  

Unlike Helm khelm allows usage of any repository when `repositories.yaml` is not present or `--trust-any-repo` is enabled.

## Helm support

* Helm 2 is supported by the `v1` module version.
* Helm 3 is supported by the `v2` module version.

## Build and test

Build and test the khelm binary (requires Go 1.13) as well as the container image:
```sh
make clean khelm test check image e2e-test
```
_The dynamic binary is written to `build/bin/khelm` and the static binary to `build/bin/khelm-static`_.

Alternatively a static binary can be built using `docker`:
```sh
make khelm-static
```

Install the binary on your host at `/usr/local/bin/khelm`:
```sh
sudo make install
```
