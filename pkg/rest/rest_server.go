package rest

import (
	"context"
	"fmt"
	"net/http"

	echo "github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/oam-dev/velacp/pkg/log"
	initClient "github.com/oam-dev/velacp/pkg/rest/client"
	"github.com/oam-dev/velacp/pkg/rest/services"
)

var _ RestServer = &restServer{}

type Config struct {
	Port int
}

type RestServer interface {
	Run(context.Context) error
}

type restServer struct {
	server    *echo.Echo
	k8sClient client.Client
	cfg       Config
}

const (
	DefaultUINamespace = "velaux-system"
)

func New(cfg Config) (RestServer, error) {
	client, err := initClient.NewK8sClient()
	if err != nil {
		return nil, fmt.Errorf("create client for clusterService failed")
	}
	s := &restServer{
		server:    newEchoInstance(),
		k8sClient: client,
		cfg:       cfg,
	}

	return s, nil
}

func newEchoInstance() *echo.Echo {
	e := echo.New()
	e.HideBanner = true

	e.Use(middleware.Gzip())
	e.Use(middleware.Logger())
	e.Pre(middleware.RemoveTrailingSlash())

	e.Use(middleware.CORSWithConfig(middleware.CORSConfig{
		AllowOrigins:     []string{"*"},
		AllowMethods:     []string{http.MethodGet, http.MethodHead, http.MethodPut, http.MethodPatch, http.MethodPost, http.MethodDelete},
		AllowCredentials: true,
		MaxAge:           86400,
	}))

	return e
}

func (s *restServer) Run(ctx context.Context) error {
	err := s.registerServices()
	if err != nil {
		return err
	}
	return s.startHTTP(ctx)
}

func (s *restServer) registerServices() error {

	// create specific namespace for better resource management
	var ns v1.Namespace
	if err := s.k8sClient.Get(context.Background(), types.NamespacedName{Name: DefaultUINamespace}, &ns); err != nil && apierrors.IsNotFound(err) {
		// not found
		ns = v1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: DefaultUINamespace,
			},
		}
		err := s.k8sClient.Create(context.Background(), &ns)
		if err != nil {
			log.Logger.Errorf("create namespace for velaux system failed %s ", err.Error())
			return err
		}
	}
	// init common client for all service
	commonClient, err := initClient.NewK8sClient()
	if err != nil {
		return err
	}

	// capability
	capabilityService := services.NewCapabilityService(commonClient)
	s.server.GET("/capabilities", capabilityService.ListCapabilities)
	s.server.GET("/capabilities/:capabilityName", capabilityService.GetCapability)
	s.server.POST("/capabilities/:capabilityName/install", capabilityService.InstallCapability)

	// catalog
	catalogService := services.NewCatalogService(commonClient)
	s.server.GET("/catalogs", catalogService.ListCatalogs)
	s.server.POST("/catalogs", catalogService.AddCatalog)
	s.server.PUT("/catalogs", catalogService.UpdateCatalog)
	s.server.GET("/catalogs/:catalogName", catalogService.GetCatalog)
	s.server.DELETE("/catalogs/:catalogName", catalogService.DelCatalog)

	// cluster
	clusterService := services.NewClusterService(commonClient)
	s.server.GET("/cluster", clusterService.GetCluster)
	s.server.GET("/clusters", clusterService.ListClusters)
	s.server.GET("/clusternames", clusterService.GetClusterNames)
	s.server.POST("/clusters", clusterService.AddCluster)
	s.server.PUT("/clusters", clusterService.UpdateCluster)
	s.server.DELETE("/clusters/:clusterName", clusterService.DelCluster)

	// definition
	s.server.GET("/clusters/:clusterName/componentdefinitions", clusterService.ListComponentDef)
	s.server.GET("/clusters/:clusterName/traitdefinitions", clusterService.ListTraitDef)

	// application
	applicationService := services.NewApplicationService(commonClient)
	s.server.GET("/clusters/:cluster/applications", applicationService.GetApplications)
	s.server.GET("/clusters/:cluster/applications/:application", applicationService.GetApplicationDetail)
	s.server.POST("/clusters/:cluster/applications", applicationService.AddApplications)
	s.server.POST("/clusters/:cluster/appYaml", applicationService.AddApplicationYaml)
	s.server.PUT("/clusters/:cluster/applications", applicationService.UpdateApplications)
	s.server.DELETE("/clusters/:cluster/applications/:application", applicationService.RemoveApplications)

	// install KubeVela in helm way
	velaInstallService := services.NewVelaInstallService(commonClient)
	s.server.GET("/clusters/:cluster/installvela", velaInstallService.InstallVela)
	s.server.GET("/clusters/:cluster/isvelainstalled", velaInstallService.IsVelaInstalled)

	// show Definition schema
	schemaService := services.NewSchemaService(commonClient)
	s.server.GET("/clusters/:cluster/schema", schemaService.GetWorkloadSchema)

	return nil
}

func (s *restServer) startHTTP(ctx context.Context) error {
	// Start HTTP server
	log.Logger.Infof("HTTP APIs are being served on port: %d", s.cfg.Port)
	addr := fmt.Sprintf(":%d", s.cfg.Port)
	return s.server.Start(addr)
}
