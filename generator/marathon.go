package generator

import (
	"github.com/ContainX/beethoven/config"
	"github.com/ContainX/beethoven/tracker"
	"github.com/ContainX/depcon/marathon"
	"github.com/ContainX/depcon/pkg/logger"
	"strings"
	"time"
)

type Generator struct {
	cfg          *config.Config
	tracker      *tracker.Tracker
	handler      func(proxyConf string)
	events       marathon.EventsChannel
	marathon     marathon.Marathon
	shutdown     ShutdownChan
	reloadQueue  ReloadChan
	templateData TemplateData
}

type ShutdownChan chan bool
type ReloadChan chan bool

var (
	log = logger.GetLogger("beethoven.generator")
)

func New(cfg *config.Config, tracker *tracker.Tracker) *Generator {
	return &Generator{
		cfg:          cfg,
		tracker:      tracker,
		shutdown:     make(ShutdownChan, 2),
		reloadQueue:  make(ReloadChan, 2),
		templateData: TemplateData{},
	}
}

// Watch marathon for changes using streams and make callbacks to the specified
// handler when apps have been added, removed or health changes.
func (g *Generator) Watch(handler func(proxyConf string)) {
	g.handler = handler

	// MVP - no health checks - should verify and use healthy masters
	g.marathon = marathon.NewMarathonClient(g.cfg.MarthonUrls[0], g.cfg.Username, g.cfg.Password)

	// suppress marathon debug
	logger.SetLevel(logger.WARNING, "client")
	logger.SetLevel(logger.WARNING, "depcon.marathon")

	g.initSSEStream()
	g.generateConfig()
	go g.initReloadWatcher()
}

// Watches the reload channel and generated a new config
func (g *Generator) initReloadWatcher() {
	throttle := time.NewTicker(2 * time.Second)
	for {
		select {
		case <-throttle.C:
			<-g.reloadQueue
			log.Info("configuration reload triggered")
			g.generateConfig()
		}
	}
}

func (g *Generator) generateConfig() {
	if err := g.buildAppMeta(); err != nil {
		log.Error("Skipping config generation...")
		g.tracker.SetError(err)
		return
	}

	changed, err := g.writeConfiguration()
	if err != nil {
		log.Error(err.Error())
		g.tracker.SetError(err)
		return
	}

	if changed {
		log.Info("Reloading NGINX")
		err = g.reload()
		if err != nil {
			log.Error(err.Error())
			g.tracker.SetError(err)
			return
		}
	}

	// No errors - clear tracker
	g.tracker.SetError(nil)
}

// buildAppMeta Builds the app metadata used within our templates.  It is responsible
// for fetching apps and tasks and remove tasks that are not healthy or the application
// all together if their are no serviceable tasks
func (g *Generator) buildAppMeta() error {
	apps, err := g.marathon.ListApplicationsWithFilters("embed=apps.tasks")
	if err != nil {
		log.Error("Error fetching apps: %s", err.Error())
		return err
	}

	// Reset current context since the config won't be rewritten until syntax
	// and validation occurs
	g.templateData.Apps = map[string]*App{}

	for _, a := range apps.Apps {

		// Create template based app
		tapp := new(App)
		tapp.AppId = appIdToDashes(a.ID)
		tapp.Env = a.Env
		tapp.Labels = a.Labels
		tapp.Tasks = []Task{}

		// Iterate through the apps tasks - remove any tasks that do not match
		// our criteria for being healthy
		for _, t := range a.Tasks {
			// Skip tasks with no ports
			if len(t.Ports) == 0 {
				continue
			}

			// If health checks defined, make sure they are passing
			if a.HealthChecks != nil && len(a.HealthChecks) > 0 {
				if t.HealthCheckResult == nil || len(t.HealthCheckResult) == 0 {
					// currently deploying - no health checks yet
					continue
				}

				// If any health check is failing lets assume failure altogether
				healthy := true
				for _, hc := range t.HealthCheckResult {
					if hc.Alive == false {
						healthy = false
					}
				}

				if healthy == false {
					continue
				}
			}
			tapp.Tasks = append(tapp.Tasks, marathonTaskToTask(t))
		}

		// Only add apps with tasks
		if len(tapp.Tasks) > 0 {
			g.templateData.Apps[tapp.AppId] = tapp
		}

	}
	g.tracker.SetLastSync(time.Now())
	return nil
}

// Translate Marathon IDs using /'s to '-' since we need identifiers
// that are compat with templates.
// ex: /products/stores/someservice would be products-stores-someservice
func appIdToDashes(appId string) string {
	parts := strings.Split(appId[1:], "/")
	return strings.Join(parts, "-")
}
