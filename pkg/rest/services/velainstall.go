package services

import (
	"context"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ghodss/yaml"
	"github.com/gofrs/flock"
	echo "github.com/labstack/echo/v4"
	"github.com/pkg/errors"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/cli"
	"helm.sh/helm/v3/pkg/cli/values"
	"helm.sh/helm/v3/pkg/downloader"
	"helm.sh/helm/v3/pkg/getter"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/repo"
	"helm.sh/helm/v3/pkg/storage/driver"
	appv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	apiErrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/oam-dev/velacp/pkg/log"
	"github.com/oam-dev/velacp/pkg/proto/model"
	"github.com/oam-dev/velacp/pkg/rest/apis"
	"github.com/oam-dev/velacp/pkg/runtime"
)

type VelaInstallService struct {
	k8sClient client.Client
}

func NewVelaInstallService(client client.Client) *VelaInstallService {

	return &VelaInstallService{
		k8sClient: client,
	}
}

func (s *VelaInstallService) InstallVela(c echo.Context) error {
	clusterName := c.Param("cluster")
	helmRepo := c.QueryParam("helmrepo")
	installVersion := c.QueryParam("version")

	if clusterName == "" || helmRepo == "" {
		return fmt.Errorf("get params err: cluster: %s, helmrepo: %s", clusterName, helmRepo)
	}

	var cm v1.ConfigMap
	err := s.k8sClient.Get(context.Background(), client.ObjectKey{Namespace: DefaultUINamespace, Name: clusterName}, &cm)
	if err != nil {
		return err
	}

	settings := cli.New()
	repoName := "kubevela"
	if err := AddHelmRepo(repoName, helmRepo, settings); err != nil {
		return err
	}
	if err := UpdateHelmRepo(settings); err != nil {
		return err
	}
	version, err := InstallHelmChart("kubevela", repoName, "vela-core", installVersion, cm.Data["Kubeconfig"], settings)
	if err != nil {
		return err
	}

	return c.JSON(http.StatusOK, model.InstallVelaResponse{
		Version: fmt.Sprintf("%d", version),
	})
}

func (s *VelaInstallService) IsVelaInstalled(c echo.Context) error {
	var clusterName string
	if clusterName = c.Param("cluster"); clusterName == "" {
		return fmt.Errorf("get param error: cluster: %s", clusterName)
	}

	var cm v1.ConfigMap
	err := s.k8sClient.Get(context.Background(), client.ObjectKey{Namespace: DefaultUINamespace, Name: clusterName}, &cm)
	if err != nil {
		return err
	}

	var (
		velaNamespace = "vela-system"
		velaName      = "kubevela"
		kubeConf      = cm.Data["Kubeconfig"]
	)
	helmExist, err := CheckVelaHelmChartExist(kubeConf, velaNamespace, velaName)
	if err != nil {
		return err
	}
	if helmExist {
		return c.JSON(http.StatusOK, apis.ClusterVelaStatus{Installed: true})
	}

	velaControllerExist := false
	if velaControllerExist, err = CheckVelaControllerExist(kubeConf); err != nil {
		return err
	}

	return c.JSON(http.StatusOK, apis.ClusterVelaStatus{Installed: velaControllerExist})
}

// AddHelmRepo adds repo with given name and url
func AddHelmRepo(name, url string, settings *cli.EnvSettings) error {
	f := "AddHelmRepo"
	repoFile := settings.RepositoryConfig

	err := os.MkdirAll(filepath.Dir(repoFile), os.ModePerm)
	if err != nil && !os.IsExist(err) {
		return fmt.Errorf("%s, make repo dir err: %s", f, err.Error())
	}

	// Acquire a file lock for process synchronization
	fileLock := flock.New(strings.Replace(repoFile, filepath.Ext(repoFile), ".lock", 1))
	lockCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	locked, err := fileLock.TryLockContext(lockCtx, time.Second)
	if err == nil && locked {
		//nolint:errcheck
		defer fileLock.Unlock()
	}
	if err != nil {
		return fmt.Errorf("%s, lock file err: %s", f, err.Error())
	}

	b, err := ioutil.ReadFile(repoFile)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("%s, read file err: %s", f, err.Error())
	}

	var file repo.File
	if err := yaml.Unmarshal(b, &file); err != nil {
		return fmt.Errorf("%s, unmarshal err: %s", f, err.Error())
	}

	if file.Has(name) {
		log.Logger.Infof("repository name (%s) already exists\n", name)
		return nil
	}

	c := repo.Entry{
		Name: name,
		URL:  url,
	}

	r, err := repo.NewChartRepository(&c, getter.All(settings))
	if err != nil {
		return fmt.Errorf("%s, new chart repo err: %s", f, err.Error())
	}

	if _, err := r.DownloadIndexFile(); err != nil {
		return fmt.Errorf("looks like %q is not a valid chart repository or cannot be reached, err: %s", url, err.Error())
	}

	file.Update(&c)

	if err := file.WriteFile(repoFile, 0644); err != nil {
		return fmt.Errorf("%s, write file err: %s", f, err.Error())
	}

	log.Logger.Infof("%q has been added to repositories\n", name)
	return nil
}

// UpdateHelmRepo updates charts for all helm repos
func UpdateHelmRepo(settings *cli.EnvSettings) error {
	f := "UpdateHelmRepo"
	repoFile := settings.RepositoryConfig

	file, err := repo.LoadFile(repoFile)
	if os.IsNotExist(errors.Cause(err)) || len(file.Repositories) == 0 {
		return errors.New("no repositories found. You must add one before updating")
	}
	var repos []*repo.ChartRepository
	for _, cfg := range file.Repositories {
		r, err := repo.NewChartRepository(cfg, getter.All(settings))
		if err != nil {
			return fmt.Errorf("%s, new chart repo err: %s", f, err.Error())
		}
		repos = append(repos, r)
	}

	log.Logger.Infof("Hang tight while we grab the latest from your chart repositories...")
	var wg sync.WaitGroup
	for _, re := range repos {
		wg.Add(1)
		go func(re *repo.ChartRepository) {
			defer wg.Done()
			if _, err := re.DownloadIndexFile(); err != nil {
				log.Logger.Infof("...Unable to get an update from the %q chart repository (%s):\n\t%s", re.Config.Name, re.Config.URL, err)
			} else {
				log.Logger.Infof("...Successfully got an update from the %q chart repository", re.Config.Name)
			}
		}(re)
	}
	wg.Wait()

	log.Logger.Infof("Update repo complete")
	return nil
}

func InstallHelmChart(name, repo, chart, version string, kubeConfig string, settings *cli.EnvSettings) (int, error) {
	f := "InstallHelmChart"

	cli, err := runtime.GetClient([]byte(kubeConfig))
	if err != nil {
		return 0, err
	}

	velaNamespace := &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "vela-system"}}
	if err := cli.Create(context.TODO(), velaNamespace); err != nil && !apiErrors.IsAlreadyExists(err) {
		return 0, fmt.Errorf("create vela namespace error: %v", err)
	}

	config, err := clientcmd.RESTConfigFromKubeConfig([]byte(kubeConfig))
	if err != nil {
		return 0, fmt.Errorf("get restconfig from kubeconfig bytes failed : %v", err)
	}
	actionConfig, err := getActionConfig("vela-system", config)
	if err != nil {
		return 0, fmt.Errorf("get action config failed : %v", err)
	}

	client := action.NewInstall(actionConfig)
	client.Version = version

	if client.Version == "" && client.Devel {
		client.Version = ">0.0.0-0"
	}
	client.ReleaseName = name
	cp, err := client.ChartPathOptions.LocateChart(fmt.Sprintf("%s/%s", repo, chart), settings)
	if err != nil {
		return 0, fmt.Errorf("%s, locate chart err: %s", f, err.Error())
	}

	p := getter.All(settings)
	valueOpts := &values.Options{}
	vals, err := valueOpts.MergeValues(p)
	if err != nil {
		return 0, fmt.Errorf("%s, merge values err: %s", f, err.Error())
	}

	// Check chart dependencies to make sure all are present in /charts
	chartRequested, err := loader.Load(cp)
	if err != nil {
		return 0, fmt.Errorf("%s, locate chart err: %s", f, err.Error())
	}

	validInstallableChart, err := isChartInstallable(chartRequested)
	if !validInstallableChart {
		return 0, err
	}

	if req := chartRequested.Metadata.Dependencies; req != nil {
		// If CheckDependencies returns an error, have unfulfilled dependencies.
		if err := action.CheckDependencies(chartRequested, req); err != nil {
			if client.DependencyUpdate {
				man := &downloader.Manager{
					Out:              os.Stdout,
					ChartPath:        cp,
					Keyring:          client.ChartPathOptions.Keyring,
					SkipUpdate:       false,
					Getters:          p,
					RepositoryConfig: settings.RepositoryConfig,
					RepositoryCache:  settings.RepositoryCache,
				}
				if err := man.Update(); err != nil {
					return 0, fmt.Errorf("%s, dependency update err: %s", f, err.Error())
				}
			}
		}
	}

	client.Namespace = settings.Namespace()
	release, err := client.Run(chartRequested, vals)
	if err != nil {
		return 0, fmt.Errorf("%s, exec err: %s", f, err.Error())
	}

	log.Logger.Infof("install complete")
	return release.Version, nil
}

func CheckVelaControllerExist(kubeConfig string) (bool, error) {
	k8sCli, err := runtime.GetClient([]byte(kubeConfig))
	if err != nil {
		return false, err
	}

	objectKey := client.ObjectKey{Namespace: "vela-system", Name: "kubevela-vela-core"}
	if err := k8sCli.Get(context.TODO(), objectKey, &appv1.Deployment{}); err != nil {
		if apiErrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}

	return true, nil
}

func CheckVelaHelmChartExist(kubeConfig string, namespace string, name string) (bool, error) {
	rel, err := GetHelmChartRelease(kubeConfig, namespace, name)

	if err != nil && !errors.Is(err, driver.ErrReleaseNotFound) {
		return false, err
	}

	return rel != nil, nil
}

func GetHelmChartRelease(kubeConfig string, namespace string, name string) (*release.Release, error) {
	config, err := clientcmd.RESTConfigFromKubeConfig([]byte(kubeConfig))
	if err != nil {
		return nil, fmt.Errorf("build restConfig error: %v", err)
	}

	actionConfig, err := getActionConfig(namespace, config)
	if err != nil {
		return nil, err
	}

	helmCli := action.NewStatus(actionConfig)
	rel, err := helmCli.Run(name)
	if err != nil {
		return nil, err
	}
	return rel, nil
}

func getActionConfig(namespace string, config *rest.Config) (*action.Configuration, error) {
	actionConfig := new(action.Configuration)

	var kubeConfig = genericclioptions.NewConfigFlags(false)
	kubeConfig.APIServer = &config.Host
	kubeConfig.BearerToken = &config.BearerToken
	kubeConfig.CAFile = &config.CAFile
	kubeConfig.Namespace = &namespace

	return actionConfig, actionConfig.Init(kubeConfig, namespace, "", loggerFunc)
}

func isChartInstallable(ch *chart.Chart) (bool, error) {
	switch ch.Metadata.Type {
	case "", "application":
		return true, nil
	}
	return false, errors.Errorf("%s charts are not installable", ch.Metadata.Type)
}

func loggerFunc(format string, v ...interface{}) {
	log.Logger.Infof(format, v...)
}
