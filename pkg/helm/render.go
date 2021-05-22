package helm

import (
	"bytes"
	"context"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/mgoltzsche/khelm/internal/matcher"
	"github.com/mgoltzsche/khelm/pkg/config"
	"github.com/pkg/errors"
	"k8s.io/helm/pkg/chartutil"
	"k8s.io/helm/pkg/getter"
	"k8s.io/helm/pkg/manifest"
	"k8s.io/helm/pkg/proto/hapi/chart"
	"k8s.io/helm/pkg/renderutil"
	"sigs.k8s.io/kustomize/kyaml/yaml"
)

var whitespaceRegex = regexp.MustCompile(`^\s*$`)

// Render manifest from helm chart configuration (shorthand)
func (h *Helm) Render(ctx context.Context, req *config.ChartConfig) (r []*yaml.RNode, err error) {
	if errs := req.Validate(); len(errs) > 0 {
		return nil, errors.Errorf("invalid chart renderer config:\n * %s", strings.Join(errs, "\n * "))
	}
	wd, err := os.Getwd()
	if err != nil {
		return nil, errors.WithStack(err)
	}
	if req.BaseDir == "" {
		req.BaseDir = wd
	} else if !filepath.IsAbs(req.BaseDir) {
		req.BaseDir = filepath.Join(wd, req.BaseDir)
	}

	chartRequested, err := h.loadChart(ctx, req)
	if err != nil {
		return nil, errors.Wrapf(err, "load chart %s", req.Chart)
	}

	log.Printf("Rendering chart %s %s with name %q and namespace %q", chartRequested.Metadata.Name, chartRequested.Metadata.Version, req.Name, req.Namespace)

	ch := make(chan struct{}, 1)
	go func() {
		r, err = renderChart(chartRequested, req, h.Getters)
		ch <- struct{}{}
	}()
	select {
	case <-ch:
		return r, err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// renderChart renders a manifest from the given chart and values
// Derived from https://github.com/helm/helm/blob/v2.14.3/cmd/helm/template.go
func renderChart(chrt *chart.Chart, req *config.ChartConfig, getters getter.Providers) (r []*yaml.RNode, err error) {
	namespace := req.Namespace
	renderOpts := renderutil.Options{
		ReleaseOptions: chartutil.ReleaseOptions{
			Name:      req.Name,
			Namespace: namespace,
			IsInstall: true,
		},
		KubeVersion: req.KubeVersion,
	}
	if len(req.APIVersions) > 0 {
		renderOpts.APIVersions = append(req.APIVersions, "v1")
	}
	rawVals, err := vals(chrt, req.ValueFiles, req.Values, req.BaseDir, getters, "", "", "")
	if err != nil {
		return nil, errors.Wrapf(err, "load values for chart %s", chrt.Metadata.Name)
	}
	config := &chart.Config{Raw: string(rawVals), Values: map[string]*chart.Value{}}

	renderedTemplates, err := renderutil.Render(chrt, config, renderOpts)
	if err != nil {
		return nil, errors.Wrapf(err, "render chart %s", chrt.Metadata.Name)
	}

	manifests := manifest.SplitManifests(renderedTemplates)

	if len(manifests) == 0 {
		return nil, errors.Errorf("chart %s does not contain any manifests", chrt.Metadata.Name)
	}

	inclusions := matcher.Any()
	if len(req.Include) > 0 {
		inclusions = matcher.FromResourceSelectors(req.Include)
	}

	transformer := manifestTransformer{
		ForceNamespace: req.ForceNamespace,
		Includes:       inclusions,
		Excludes:       matcher.FromResourceSelectors(req.Exclude),
		NamespacedOnly: req.NamespacedOnly,
	}
	chartHookMatcher := matcher.NewChartHookMatcher(transformer.Excludes, !req.ExcludeHooks)
	transformer.Excludes = chartHookMatcher

	r = make([]*yaml.RNode, 0, len(manifests))
	for _, m := range sortByKind(manifests) {
		b := filepath.Base(m.Name)
		if b == "NOTES.txt" || strings.HasPrefix(b, "_") || whitespaceRegex.MatchString(m.Content) {
			continue
		}
		transformed, err := transformer.TransformManifest(bytes.NewReader([]byte(m.Content)))
		if err != nil {
			return nil, errors.WithMessage(err, filepath.Base(m.Name))
		}
		r = append(r, transformed...)
	}

	if err = transformer.Includes.RequireAllMatched(); err != nil {
		return nil, errors.Wrap(err, "resource inclusion")
	}

	if err = transformer.Excludes.RequireAllMatched(); err != nil {
		return nil, errors.Wrap(err, "resource exclusion")
	}

	if len(r) == 0 {
		return nil, errors.Errorf("no output since all resources were excluded")
	}

	if hooks := chartHookMatcher.FoundHooks(); !req.ExcludeHooks && len(hooks) > 0 {
		log.Printf("WARNING: The chart output contains the following hooks: %s", strings.Join(hooks, ", "))
	}

	return
}
