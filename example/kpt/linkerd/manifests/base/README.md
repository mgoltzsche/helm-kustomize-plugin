# Raw Helm chart output

This directory contains the linkerd chart output at `generated-manifest-without-secrets.yaml` which is generated by khelm as specified in `../helm-kustomize-pipeline.yaml`.
The chart is rendered with fake certificates and serves as intermediate kustomization that can be enhanced with cert-manager Certificates.
However the (fake) base64 encoded CA certificate is still contained within the output manifest as a webhook's `caBundle` or within environment variables which must be replaced.
Therefore the intermediate generated manifest `generated-manifest-without-secrets.yaml` is not versioned here.
