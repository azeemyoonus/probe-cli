package nettests

import (
	"time"

	"github.com/apex/log"
	"github.com/ooni/probe-cli/v3/cmd/ooniprobe/internal/database"
	"github.com/ooni/probe-cli/v3/cmd/ooniprobe/internal/ooni"
	"github.com/pkg/errors"
)

// RunGroupConfig contains the settings for running a nettest group.
type RunGroupConfig struct {
	GroupName  string
	Probe      *ooni.Probe
	InputFiles []string
	Inputs     []string
}

const websitesURLLimitRemoved = `WARNING: CONFIGURATION CHANGE REQUIRED:

* Since ooniprobe 3.9.0, websites_url_limit has been replaced
  by websites_max_runtime in the configuration

* To silence this warning either set websites_url_limit to zero or
  replace it with websites_max_runtime

* For the rest of 2021, we will automatically convert websites_url_limit
  to websites_max_runtime (if the latter is not already set)

* We will consider that each URL in websites_url_limit takes five
  seconds to run and thus calculate websites_max_runtime

* Since 2022, we will start silently ignoring websites_url_limit
`

// RunGroup runs a group of nettests according to the specified config.
func RunGroup(config RunGroupConfig) error {
	if config.Probe.Config().Nettests.WebsitesURLLimit > 0 {
		log.Warn(websitesURLLimitRemoved)
		if config.Probe.Config().Nettests.WebsitesMaxRuntime <= 0 {
			limit := config.Probe.Config().Nettests.WebsitesURLLimit
			maxRuntime := 5 * limit
			config.Probe.Config().Nettests.WebsitesMaxRuntime = maxRuntime
		}
		time.Sleep(30 * time.Second)
	}

	if config.Probe.IsTerminated() {
		log.Debugf("context is terminated, stopping runNettestGroup early")
		return nil
	}

	sess, err := config.Probe.NewSession()
	if err != nil {
		log.WithError(err).Error("Failed to create a measurement session")
		return err
	}
	defer sess.Close()

	err = sess.MaybeLookupLocation()
	if err != nil {
		log.WithError(err).Error("Failed to lookup the location of the probe")
		return err
	}
	network, err := database.CreateNetwork(config.Probe.DB(), sess)
	if err != nil {
		log.WithError(err).Error("Failed to create the network row")
		return err
	}
	if err := sess.MaybeLookupBackends(); err != nil {
		log.WithError(err).Warn("Failed to discover OONI backends")
		return err
	}

	group, ok := All[config.GroupName]
	if !ok {
		log.Errorf("No test group named %s", config.GroupName)
		return errors.New("invalid test group name")
	}
	log.Debugf("Running test group %s", group.Label)

	result, err := database.CreateResult(
		config.Probe.DB(), config.Probe.Home(), config.GroupName, network.ID)
	if err != nil {
		log.Errorf("DB result error: %s", err)
		return err
	}

	config.Probe.ListenForSignals()
	config.Probe.MaybeListenForStdinClosed()
	for i, nt := range group.Nettests {
		if config.Probe.IsTerminated() == true {
			log.Debugf("context is terminated, stopping group.Nettests early")
			break
		}
		log.Debugf("Running test %T", nt)
		ctl := NewController(nt, config.Probe, result, sess)
		ctl.InputFiles = config.InputFiles
		ctl.Inputs = config.Inputs
		ctl.SetNettestIndex(i, len(group.Nettests))
		if err = nt.Run(ctl); err != nil {
			log.WithError(err).Errorf("Failed to run %s", group.Label)
		}
	}

	if err = result.Finished(config.Probe.DB()); err != nil {
		return err
	}
	return nil
}
