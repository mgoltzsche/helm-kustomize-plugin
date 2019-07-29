package helm

import (
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/ghodss/yaml"
	"github.com/pkg/errors"
	"k8s.io/helm/pkg/chartutil"
	"k8s.io/helm/pkg/downloader"
	"k8s.io/helm/pkg/getter"
	"k8s.io/helm/pkg/helm/environment"
	"k8s.io/helm/pkg/helm/helmpath"
	"k8s.io/helm/pkg/manifest"
	"k8s.io/helm/pkg/proto/hapi/chart"
	"k8s.io/helm/pkg/renderutil"
	"k8s.io/helm/pkg/repo"
	//"github.com/ContainerSolutions/helm-convert/pkg/helm"
)

/*func Render(loadCfg *helm.LoadChartConfig, renderCfg *helm.RenderChartConfig, writer io.Writer) (err error) {
	var (
		settings environment.EnvSettings
	)
	settings.Home = "/home/max/.helm"
	h := helm.NewHelm(settings, writer)
	chartRequested, err := h.LoadChart(loadCfg)
	if err != nil {
		return
	}
	if renderCfg.Name == "" {
		renderCfg.Name = chartRequested.Metadata.Name
	}
	renderedManifests, err := h.RenderChart(renderCfg)
	if err != nil {
		return
	}
	for _, m := range renderedManifests {
		fmt.Fprintln(writer, m.Content)
	}
	return
}*/

// derived from https://github.com/ContainerSolutions/helm-convert/blob/v0.5.0/pkg/helm/helm.go

const stableRepository = "stable"

var (
	whitespaceRegex    = regexp.MustCompile(`^\s*$`)
	defaultKubeVersion = fmt.Sprintf("%s.%s", chartutil.DefaultKubeVersion.Major, chartutil.DefaultKubeVersion.Minor)
)

// Helm type
type Helm struct {
	settings environment.EnvSettings
	out      io.Writer
}

// GeneratorConfig define the kustomize plugin's input file content
type GeneratorConfig struct {
	APIVersion string      `yaml:"apiVersion"`
	Kind       string      `yaml:"kind"`
	Metadata   K8sMetadata `yaml:"metadata"`
	ChartConfig
}

// K8sMetadata define the name to be kubernetes object schema conform
type K8sMetadata struct {
	Name string `yaml:"name"`
}

// ChartConfig define chart lookup and render config
type ChartConfig struct {
	HelmHome string `yaml: "helmHome"`
	LoadChartConfig
	RenderConfig
}

// LoadChartConfig define the configuration to load a chart
type LoadChartConfig struct {
	Repository string `yaml:"repository"`
	Chart      string `yaml:"chart"`
	Version    string `yaml:"version"`
	Username   string `yaml:"user,omitempty"`
	Password   string `yaml:"password,omitempty"`
	DepUp      bool   `yaml:"loadDependencies,omitempty"`
	Verify     bool   `yaml:"verify,omitempty"`
	Keyring    string `yaml:"keyring,omitempty"`
	CertFile   string `yaml:"certFile,omitempty"`
	KeyFile    string `yaml:"keyFile,omitempty"`
	CaFile     string `yaml:"caFile,omitempty"`
}

// RenderConfig defines the configuration to render a chart
type RenderConfig struct {
	Name       string                 `yaml:"name,omitempty"`
	Namespace  string                 `yaml:"namespace,omitempty"`
	ValueFiles []string               `yaml:"valueFiles,omitempty"`
	Values     map[string]interface{} `yaml:"values,omitempty"`
}

// ReadGeneratorConfig read the generator configuration
func ReadGeneratorConfig(reader io.Reader) (cfg *GeneratorConfig, err error) {
	b, err := ioutil.ReadAll(reader)
	if err == nil {
		cfg = &GeneratorConfig{}
		err = yaml.Unmarshal(b, cfg)
	}
	if err == nil {
		if cfg.Repository == "" {
			err = errors.New("chart repository not specified")
		}
		if cfg.Version == "" {
			err = errors.New("version not specified")
		}
		if cfg.Chart == "" {
			err = errors.New("chart not specified")
		}
	}
	return cfg, errors.Wrap(err, "read chart inflator config")
}

// Render manifest from helm chart configuration (shorthand)
func Render(cfg *GeneratorConfig, writer io.Writer) (err error) {
	h := NewHelm("", writer)
	chrt, err := h.LoadChart(&cfg.LoadChartConfig)
	if err != nil {
		return
	}
	renderCfg := &cfg.RenderConfig
	if renderCfg.Name == "" {
		renderCfg.Name = chrt.Metadata.Name
	}
	resources, err := h.RenderChart(chrt, renderCfg)
	if err != nil {
		return
	}
	for _, m := range resources {
		b := filepath.Base(m.Name)
		if b != "NOTES.txt" && !strings.HasPrefix(b, "_") && !whitespaceRegex.MatchString(m.Content) {
			fmt.Fprintln(writer, "---\n"+m.Content)
		}
	}
	return
}

// NewHelm constructs helm
func NewHelm(home string, out io.Writer) *Helm {
	settings := environment.EnvSettings{
		Home: helmpath.Home(environment.DefaultHelmHome),
	}
	if home != "" {
		settings.Home = helmpath.Home(home)
	}
	return &Helm{
		settings,
		out,
	}
}

// Initialize initialize the helm home directory
func (h *Helm) Initialize() (err error) {
	// TODO:
	/*if _, e := os.Stat(h.settings.Home.String()); e == nil {
		return
	}*/

	log.Printf("Initializing helm home at %s\n", h.settings.Home)

	// Create directories
	home := h.settings.Home
	for _, dir := range []string{
		home.String(),
		home.Repository(),
		home.Cache(),
		home.LocalRepository(),
		home.Plugins(),
		home.Starters(),
		home.Archive(),
	} {
		if err = os.MkdirAll(dir, 0755); err != nil {
			return
		}
	}

	// Create repos
	repoFile := home.RepositoryFile()
	f := repo.NewRepoFile()
	stableRepositoryURL := "https://kubernetes-charts.storage.googleapis.com"
	repo, err := initStableRepo(home.CacheIndex(stableRepository), home, h.settings, stableRepositoryURL)
	if err != nil {
		return
	}
	f.Add(repo)
	return f.WriteFile(repoFile, 0644)
}

func initStableRepo(cacheFile string, home helmpath.Home, settings environment.EnvSettings, stableRepositoryURL string) (*repo.Entry, error) {
	c := repo.Entry{
		Name:  stableRepository,
		URL:   stableRepositoryURL,
		Cache: cacheFile,
	}
	r, err := repo.NewChartRepository(&c, getter.All(settings))
	if err != nil {
		return nil, err
	}

	if _, e := os.Stat(cacheFile); e == nil {
		return &c, nil
	}

	if err := r.DownloadIndexFile(cacheFile); err != nil {
		return nil, fmt.Errorf("Looks like %q is not a valid chart repository or cannot be reached: %s", stableRepositoryURL, err.Error())
	}

	return &c, nil
}

// LoadChart download a chart or load it from cache
func (h *Helm) LoadChart(ref *LoadChartConfig) (c *chart.Chart, err error) {
	if err = h.Initialize(); err != nil {
		return
	}

	chartPath, err := h.LocateChartPath(
		ref.Repository,
		ref.Username,
		ref.Password,
		ref.Chart,
		ref.Version,
		ref.Verify,
		ref.Keyring,
		ref.CertFile,
		ref.KeyFile,
		ref.CaFile,
	)
	if err != nil {
		return
	}
	log.Printf("Using chart path %v", chartPath)

	// Check chart requirements to make sure all dependencies are present in /charts
	if c, err = chartutil.Load(chartPath); err != nil {
		return
	}
	req, e := chartutil.LoadRequirements(c)
	if e == nil {
		if err = renderutil.CheckDependencies(c, req); err != nil {
			if ref.DepUp {
				man := &downloader.Manager{
					Out:        h.out,
					ChartPath:  chartPath,
					HelmHome:   h.settings.Home,
					Keyring:    ref.Keyring,
					SkipUpdate: false,
					Getters:    getter.All(h.settings),
				}
				if err = man.Update(); err != nil {
					return
				}

				// Update all dependencies which are present in /charts.
				c, err = chartutil.Load(chartPath)
				if err != nil {
					return
				}
			} else {
				return
			}
		}
	} else if e != chartutil.ErrRequirementsNotFound {
		return nil, fmt.Errorf("cannot load requirements: %v", e)
	}

	return
}

// RenderChart manifest
func (h *Helm) RenderChart(chrt *chart.Chart, c *RenderConfig) (m []manifest.Manifest, err error) {
	renderOpts := renderutil.Options{
		ReleaseOptions: chartutil.ReleaseOptions{
			Name:      c.Name,
			Namespace: c.Namespace,
		},
		KubeVersion: defaultKubeVersion,
	}
	log.Printf("Rendering chart with name %q, namespace: %q\n", c.Name, c.Namespace)

	rawVals, err := h.Vals(c.ValueFiles, c.Values, "", "", "")
	if err != nil {
		return nil, errors.Wrap(err, "load values")
	}
	config := &chart.Config{Raw: string(rawVals), Values: map[string]*chart.Value{}}

	renderedResources, err := renderutil.Render(chrt, config, renderOpts)
	if err != nil {
		return nil, errors.Wrap(err, "render chart")
	}

	return manifest.SplitManifests(renderedResources), nil
}

// LocateChartPath looks for a chart directory in known places, and returns either the full path or an error.
//
// This does not ensure that the chart is well-formed; only that the requested filename exists.
//
// Order of resolution:
// - current working directory
// - if path is absolute or begins with '.', error out here
// - chart repos in $HELM_HOME
// - URL
//
// If 'verify' is true, this will attempt to also verify the chart.
func (h *Helm) LocateChartPath(repoURL, username, password, name, version string, verify bool, keyring,
	certFile, keyFile, caFile string) (string, error) {
	name = strings.TrimSpace(name)
	version = strings.TrimSpace(version)
	if fi, err := os.Stat(name); err == nil {
		abs, err := filepath.Abs(name)
		if err != nil {
			return abs, err
		}
		if verify {
			if fi.IsDir() {
				return "", errors.New("cannot verify a directory")
			}
			if _, err := downloader.VerifyChart(abs, keyring); err != nil {
				return "", err
			}
		}
		return abs, nil
	}

	if filepath.IsAbs(name) || strings.HasPrefix(name, ".") {
		return name, fmt.Errorf("path %q not found", name)
	}

	crepo := filepath.Join(h.settings.Home.Repository(), name)

	if _, err := os.Stat(crepo); err == nil {
		return filepath.Abs(crepo)
	}

	dl := downloader.ChartDownloader{
		HelmHome: h.settings.Home,
		Out:      h.out,
		Keyring:  keyring,
		Getters:  getter.All(h.settings),
		Username: username,
		Password: password,
	}

	if verify {
		dl.Verify = downloader.VerifyAlways
	}

	if repoURL != "" {
		chartURL, err := repo.FindChartInAuthRepoURL(repoURL, username, password, name, version,
			certFile, keyFile, caFile, getter.All(h.settings))
		if err != nil {
			return "", err
		}
		name = chartURL
	}

	if _, err := os.Stat(h.settings.Home.Archive()); os.IsNotExist(err) {
		os.MkdirAll(h.settings.Home.Archive(), 0744)
	}

	log.Printf("Downloading chart %q version %q with user: %q, passwd: %v, keyring: %q\n", name, version, dl.Username, dl.Password != "", dl.Keyring)
	filename, _, err := dl.DownloadTo(name, version, h.settings.Home.Archive())

	if err != nil {
		return filename, err
	}

	return filepath.Abs(filename)
}

// Vals merges values from files specified via -f/--values and
// directly via --set or --set-string or --set-file, marshaling them to YAML
func (h *Helm) Vals(valueFiles []string, values map[string]interface{}, CertFile, KeyFile, CAFile string) (b []byte, err error) {
	base := map[string]interface{}{}
	for _, filePath := range valueFiles {
		currentMap := map[string]interface{}{}
		if b, err = h.readFile(filePath, CertFile, KeyFile, CAFile); err != nil {
			return
		}
		if err = yaml.Unmarshal(b, &currentMap); err != nil {
			return nil, fmt.Errorf("failed to parse %s: %s", filePath, err)
		}
		mergeValues(base, currentMap)
	}
	base = mergeValues(base, values)
	return yaml.Marshal(base)
}

//readFile load a file from the local directory or a remote file with a url.
func (h *Helm) readFile(filePath, CertFile, KeyFile, CAFile string) ([]byte, error) {
	u, _ := url.Parse(filePath)
	p := getter.All(h.settings)

	// TODO: verify that values file is within root

	// FIXME: maybe someone handle other protocols like ftp.
	getterConstructor, err := p.ByScheme(u.Scheme)

	if err != nil {
		return ioutil.ReadFile(filePath)
	}

	getter, err := getterConstructor(filePath, CertFile, KeyFile, CAFile)
	if err != nil {
		return []byte{}, err
	}
	data, err := getter.Get(filePath)
	return data.Bytes(), err
}

func mergeValues(dest map[string]interface{}, src map[string]interface{}) map[string]interface{} {
	for k, v := range src {
		// If the key doesn't exist already, then just set the key to that value
		if _, exists := dest[k]; !exists {
			dest[k] = v
			continue
		}
		nextMap, ok := v.(map[string]interface{})
		// If it isn't another map, overwrite the value
		if !ok {
			dest[k] = v
			continue
		}
		// Edge case: If the key exists in the destination, but isn't a map
		destMap, isMap := dest[k].(map[string]interface{})
		// If the source map has a map for this key, prefer it
		if !isMap {
			dest[k] = v
			continue
		}
		// If we got to this point, it is a map in both, so merge them
		dest[k] = mergeValues(destMap, nextMap)
	}
	return dest
}
