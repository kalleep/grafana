package provisioning

import (
	"context"
	"path/filepath"
	"sync"

	"github.com/grafana/grafana/pkg/infra/log"
	plugifaces "github.com/grafana/grafana/pkg/plugins"
	"github.com/grafana/grafana/pkg/registry"
	"github.com/grafana/grafana/pkg/services/provisioning/dashboards"
	"github.com/grafana/grafana/pkg/services/provisioning/datasources"
	"github.com/grafana/grafana/pkg/services/provisioning/notifiers"
	"github.com/grafana/grafana/pkg/services/provisioning/plugins"
	"github.com/grafana/grafana/pkg/services/sqlstore"
	"github.com/grafana/grafana/pkg/setting"
	"github.com/grafana/grafana/pkg/util/errutil"
)

type ProvisioningService interface {
	registry.BackgroundService
	RunInitProvisioners() error
	ProvisionDatasources() error
	ProvisionPlugins() error
	ProvisionNotifications() error
	ProvisionDashboards() error
	GetDashboardProvisionerResolvedPath(name string) string
	GetAllowUIUpdatesFromConfig(name string) bool
}

func init() {
	registry.Register(&registry.Descriptor{
		Name:         "ProvisioningService",
		Instance:     NewProvisioningServiceImpl(),
		InitPriority: registry.Low,
	})
}

// Add a public constructor for overriding service to be able to instantiate OSS as fallback
func NewProvisioningServiceImpl() *provisioningServiceImpl {
	return &provisioningServiceImpl{
		log:                     log.New("provisioning"),
		newDashboardProvisioner: dashboards.New,
		provisionNotifiers:      notifiers.Provision,
		provisionDatasources:    datasources.Provision,
		provisionPlugins:        plugins.Provision,
	}
}

// Used for testing purposes
func newProvisioningServiceImpl(
	newDashboardProvisioner dashboards.DashboardProvisionerFactory,
	provisionNotifiers func(string) error,
	provisionDatasources func(string) error,
	provisionPlugins func(string, plugifaces.Manager) error,
) *provisioningServiceImpl {
	return &provisioningServiceImpl{
		log:                     log.New("provisioning"),
		newDashboardProvisioner: newDashboardProvisioner,
		provisionNotifiers:      provisionNotifiers,
		provisionDatasources:    provisionDatasources,
		provisionPlugins:        provisionPlugins,
	}
}

type provisioningServiceImpl struct {
	Cfg                     *setting.Cfg       `inject:""`
	SQLStore                *sqlstore.SQLStore `inject:""`
	PluginManager           plugifaces.Manager `inject:""`
	log                     log.Logger
	pollingCtxCancel        context.CancelFunc
	newDashboardProvisioner dashboards.DashboardProvisionerFactory
	dashboardProvisioner    dashboards.DashboardProvisioner
	provisionNotifiers      func(string) error
	provisionDatasources    func(string) error
	provisionPlugins        func(string, plugifaces.Manager) error
	mutex                   sync.Mutex
}

func (ps *provisioningServiceImpl) Init() error {
	return nil
}

func (ps *provisioningServiceImpl) RunInitProvisioners() error {
	err := ps.ProvisionDatasources()
	if err != nil {
		return err
	}

	err = ps.ProvisionPlugins()
	if err != nil {
		return err
	}

	err = ps.ProvisionNotifications()
	if err != nil {
		return err
	}

	return nil
}

func (ps *provisioningServiceImpl) Run(ctx context.Context) error {
	err := ps.ProvisionDashboards()
	if err != nil {
		ps.log.Error("Failed to provision dashboard", "error", err)
		return err
	}

	for {
		// Wait for unlock. This is tied to new dashboardProvisioner to be instantiated before we start polling.
		ps.mutex.Lock()
		// Using background here because otherwise if root context was canceled the select later on would
		// non-deterministically take one of the route possibly going into one polling loop before exiting.
		pollingContext, cancelFun := context.WithCancel(context.Background())
		ps.pollingCtxCancel = cancelFun
		ps.dashboardProvisioner.PollChanges(pollingContext)
		ps.mutex.Unlock()

		select {
		case <-pollingContext.Done():
			// Polling was canceled.
			continue
		case <-ctx.Done():
			// Root server context was cancelled so cancel polling and leave.
			ps.cancelPolling()
			return ctx.Err()
		}
	}
}

func (ps *provisioningServiceImpl) ProvisionDatasources() error {
	datasourcePath := filepath.Join(ps.Cfg.ProvisioningPath, "datasources")
	err := ps.provisionDatasources(datasourcePath)
	return errutil.Wrap("Datasource provisioning error", err)
}

func (ps *provisioningServiceImpl) ProvisionPlugins() error {
	appPath := filepath.Join(ps.Cfg.ProvisioningPath, "plugins")
	err := ps.provisionPlugins(appPath, ps.PluginManager)
	return errutil.Wrap("app provisioning error", err)
}

func (ps *provisioningServiceImpl) ProvisionNotifications() error {
	alertNotificationsPath := filepath.Join(ps.Cfg.ProvisioningPath, "notifiers")
	err := ps.provisionNotifiers(alertNotificationsPath)
	return errutil.Wrap("Alert notification provisioning error", err)
}

func (ps *provisioningServiceImpl) ProvisionDashboards() error {
	dashboardPath := filepath.Join(ps.Cfg.ProvisioningPath, "dashboards")
	dashProvisioner, err := ps.newDashboardProvisioner(dashboardPath, ps.SQLStore)
	if err != nil {
		return errutil.Wrap("Failed to create provisioner", err)
	}

	ps.mutex.Lock()
	defer ps.mutex.Unlock()

	ps.cancelPolling()
	dashProvisioner.CleanUpOrphanedDashboards()

	err = dashProvisioner.Provision()
	if err != nil {
		// If we fail to provision with the new provisioner, the mutex will unlock and the polling will restart with the
		// old provisioner as we did not switch them yet.
		return errutil.Wrap("Failed to provision dashboards", err)
	}
	ps.dashboardProvisioner = dashProvisioner
	return nil
}

func (ps *provisioningServiceImpl) GetDashboardProvisionerResolvedPath(name string) string {
	return ps.dashboardProvisioner.GetProvisionerResolvedPath(name)
}

func (ps *provisioningServiceImpl) GetAllowUIUpdatesFromConfig(name string) bool {
	return ps.dashboardProvisioner.GetAllowUIUpdatesFromConfig(name)
}

func (ps *provisioningServiceImpl) cancelPolling() {
	if ps.pollingCtxCancel != nil {
		ps.log.Debug("Stop polling for dashboard changes")
		ps.pollingCtxCancel()
	}
	ps.pollingCtxCancel = nil
}
