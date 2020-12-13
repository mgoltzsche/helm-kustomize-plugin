package helm

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/Masterminds/semver/v3"
	"github.com/pkg/errors"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/cli"
	"helm.sh/helm/v3/pkg/downloader"
	"helm.sh/helm/v3/pkg/getter"
)

// loadChart loads chart from local or remote location
func (h *Helm) loadChart(ctx context.Context, cfg *ChartConfig) (*chart.Chart, error) {
	if cfg.Chart == "" {
		return nil, errors.New("no chart specified")
	}
	_, err := os.Stat(absPath(cfg.Chart, cfg.BaseDir))
	fileExists := err == nil
	if cfg.Repository == "" {
		if fileExists {
			return h.buildAndLoadLocalChart(ctx, cfg)
		} else if l := strings.Split(cfg.Chart, "/"); len(l) == 2 && l[0] != "" && l[1] != "" && l[0] != ".." && l[0] != "." {
			cfg.Repository = "@" + l[0]
			cfg.Chart = l[1]
		} else {
			return nil, errors.Errorf("chart directory %q not found and no repository specified", cfg.Chart)
		}
	}
	return h.loadRemoteChart(ctx, cfg)
}

func (h *Helm) loadRemoteChart(ctx context.Context, cfg *ChartConfig) (*chart.Chart, error) {
	repoURLs := map[string]struct{}{cfg.Repository: {}}
	repos, err := reposForURLs(repoURLs, h.TrustAnyRepository, &h.Settings, h.Getters)
	if err != nil {
		return nil, err
	}
	isRange, err := isVersionRange(cfg.Version)
	if err != nil {
		return nil, err
	}
	if isRange {
		if err = repos.UpdateIndex(ctx); err != nil {
			return nil, err
		}
	}
	chartPath, err := locateChart(ctx, &cfg.LoaderConfig, repos, &h.Settings, h.Getters)
	if err != nil {
		return nil, err
	}
	return loader.Load(chartPath)
}

func (h *Helm) buildAndLoadLocalChart(ctx context.Context, cfg *ChartConfig) (*chart.Chart, error) {
	chartPath := absPath(cfg.Chart, cfg.BaseDir)
	chartRequested, err := loader.Load(chartPath)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	localCharts := make([]localChart, 0, 1)
	dependencies := make([]*chart.Dependency, 0)
	needsRepoIndexUpdate, err := collectCharts(chartRequested, chartPath, cfg, &localCharts, &dependencies, 0)
	if err != nil {
		return nil, err
	}

	// Create (temporary) repository configuration that includes all dependencies
	repos, err := reposForDependencies(dependencies, h.TrustAnyRepository, &h.Settings, h.Getters)
	if err != nil {
		return nil, errors.Wrap(err, "init temp repositories.yaml")
	}
	repos, err = repos.Apply()
	if err != nil {
		return nil, err
	}
	defer repos.Close()
	settings := h.Settings
	settings.RepositoryConfig = repos.FilePath()

	// Download/update repo indices
	if needsRepoIndexUpdate {
		err = repos.UpdateIndex(ctx)
	} else {
		err = repos.DownloadIndexFilesIfNotExist(ctx)
	}
	if err != nil {
		return nil, err
	}

	// Build local charts recursively
	needsReload, err := buildLocalCharts(ctx, localCharts, &cfg.LoaderConfig, repos, &settings, h.Getters)
	if err != nil {
		return nil, errors.Wrap(err, "build/fetch dependencies")
	}
	// Reload the chart with the updated Chart.lock file
	if needsReload {
		chartRequested, err = loader.Load(chartPath)
		if err != nil {
			return nil, errors.Wrapf(err, "failed reloading chart %s after dependency download", cfg.Chart)
		}
	}
	return chartRequested, nil
}

func isVersionRange(version string) (bool, error) {
	if version == "" {
		return true, nil
	}
	c, err := semver.NewConstraint(version)
	if err != nil {
		return true, errors.Wrap(err, "chart version")
	}
	v, err := semver.NewVersion(version)
	if err != nil {
		return true, nil
	}
	return v.String() != c.String(), nil
}

type localChart struct {
	Chart             *chart.Chart
	Path              string
	LocalDependencies bool
}

func collectCharts(chartRequested *chart.Chart, chartPath string, cfg *ChartConfig, localCharts *[]localChart, deps *[]*chart.Dependency, depth int) (needsRepoIndexUpdate bool, err error) {
	if depth > 20 {
		return false, errors.New("collect local charts recursively: max depth of 20 reached - cyclic dependency?")
	}
	meta := chartRequested.Metadata
	if meta == nil {
		return false, errors.Errorf("chart %s has no metadata", chartPath)
	}
	name := fmt.Sprintf("%s %s", meta.Name, meta.Version)
	reqDeps := chartRequested.Metadata.Dependencies
	hasLocalDependencies := false
	for _, dep := range reqDeps {
		if strings.HasPrefix(dep.Repository, "file://") {
			hasLocalDependencies = true
			depChartPath := strings.TrimPrefix(dep.Repository, "file://")
			depChartPath = absPath(depChartPath, chartPath)
			depChart, err := loader.LoadDir(depChartPath)
			if err != nil {
				return false, errors.Wrapf(err, "load chart %s dependency %s from dir %s", name, dep.Name, depChartPath)
			}
			needsUpdate, err := collectCharts(depChart, depChartPath, cfg, localCharts, deps, depth+1)
			if err != nil {
				return false, errors.WithStack(err)
			}
			if needsUpdate {
				needsRepoIndexUpdate = true
			}
		} else if strings.HasPrefix(dep.Repository, "https://") || strings.HasPrefix(dep.Repository, "http://") {
			*deps = append(*deps, dep)
			if chartRequested.Lock == nil {
				// Update repo index when remote dependencies present but no lock file
				needsRepoIndexUpdate = true
			}
		}
	}
	*localCharts = append(*localCharts, localChart{
		Chart:             chartRequested,
		Path:              chartPath,
		LocalDependencies: hasLocalDependencies,
	})
	return needsRepoIndexUpdate, nil
}

func buildLocalCharts(ctx context.Context, localCharts []localChart, cfg *LoaderConfig, repos repositoryConfig, settings *cli.EnvSettings, getters getter.Providers) (needsReload bool, err error) {
	for _, ch := range localCharts {
		if err = action.CheckDependencies(ch.Chart, ch.Chart.Metadata.Dependencies); err != nil || ch.LocalDependencies {
			needsReload = true
			meta := ch.Chart.Metadata
			if meta == nil {
				return false, errors.Errorf("chart %s has no metadata", ch.Path)
			}
			name := fmt.Sprintf("%s %s", meta.Name, meta.Version)
			log.Printf("Building/fetching chart %s dependencies", name)
			if err = buildChartDependencies(ctx, ch.Chart, ch.Path, cfg, repos, settings, getters); err != nil {
				return false, errors.Wrapf(err, "build chart %s", name)
			}
		}
	}
	return needsReload, nil
}

func dependencyFilePath(chartPath string, d *chart.Dependency) string {
	name := fmt.Sprintf("%s-%s.tgz", d.Name, d.Version)
	return filepath.Join(chartPath, "charts", name)
}

func buildChartDependencies(ctx context.Context, chartRequested *chart.Chart, chartPath string, cfg *LoaderConfig, repos repositoryConfig, settings *cli.EnvSettings, getters getter.Providers) error {
	man := &downloader.Manager{
		Out:              log.Writer(),
		ChartPath:        chartPath,
		Keyring:          cfg.Keyring,
		SkipUpdate:       true,
		Getters:          getters,
		RepositoryConfig: settings.RepositoryConfig,
		RepositoryCache:  settings.RepositoryCache,
		Debug:            settings.Debug,
	}
	if cfg.Verify {
		man.Verify = downloader.VerifyAlways
	}
	// Workaround for leftover tmpcharts dir (which makes Build() fail)
	err := os.RemoveAll(filepath.Join(chartPath, "tmpcharts"))
	if err != nil {
		return errors.WithStack(err)
	}
	// Downloads dependencies - respecting requirements.lock if present
	err = man.Build()
	if err != nil && errors.Cause(err).Error() == "entry not found" {
		if err = repos.UpdateIndex(ctx); err == nil {
			err = man.Build()
		}
	}
	return errors.WithStack(err)
}
