package unifipoller

import (
	"log"
	"strings"
	"time"

	"github.com/golift/unifi"
	influx "github.com/influxdata/influxdb1-client/v2"
	"github.com/pkg/errors"
)

// CheckSites makes sure the list of provided sites exists on the controller.
// This does not run in Lambda (run-once) mode.
func (u *UnifiPoller) CheckSites() error {
	if strings.Contains(strings.ToLower(u.Mode), "lambda") {
		return nil // Skip this in lambda mode.
	}
	sites, err := u.GetSites()
	if err != nil {
		return err
	}
	msg := []string{}
	for _, site := range sites {
		msg = append(msg, site.Name+" ("+site.Desc+")")
	}
	u.Logf("Found %d site(s) on controller: %v", len(msg), strings.Join(msg, ", "))
	if StringInSlice("all", u.Sites) {
		u.Sites = []string{"all"}
		return nil
	}
FIRST:
	for _, s := range u.Sites {
		for _, site := range sites {
			if s == site.Name {
				continue FIRST
			}
		}
		// This is fine, it may get added later.
		u.LogErrorf("configured site not found on controller: %v", s)
	}
	return nil
}

// PollController runs forever, polling UniFi, and pushing to influx.
// This is started by Run() after everything checks out.
func (u *UnifiPoller) PollController() error {
	log.Println("[INFO] Everything checks out! Poller started, interval:", u.Interval.Round(time.Second))
	ticker := time.NewTicker(u.Interval.Round(time.Second))
	for range ticker.C {
		_ = u.CollectAndReport()
		if u.MaxErrors >= 0 && u.errorCount > u.MaxErrors {
			return errors.Errorf("reached maximum error count, stopping poller (%d > %d)", u.errorCount, u.MaxErrors)
		}
	}
	return nil
}

// CollectAndReport collects measurements and reports them to influxdb.
// Can be called once or in a ticker loop. This function and all the ones below
// handle their own logging. An error is returned so the calling function may
// determine if there was a read or write erorr and act on it. This is currently
// called in two places in this library. One returns an error, one does not.
func (u *UnifiPoller) CollectAndReport() error {
	metrics, err := u.CollectMetrics()
	if err != nil {
		return err
	}
	err = u.ReportMetrics(metrics)
	u.LogError(err, "reporting metrics")
	return err
}

// CollectMetrics grabs all the measurements from a UniFi controller and returns them.
// This also creates an InfluxDB writer, and returns an error if that fails.
func (u *UnifiPoller) CollectMetrics() (*Metrics, error) {
	m := &Metrics{}
	var err error
	// Get the sites we care about.
	m.Sites, err = u.GetFilteredSites()
	u.LogError(err, "unifi.GetSites()")
	// Get all the points.
	m.Clients, err = u.GetClients(m.Sites)
	u.LogError(err, "unifi.GetClients()")
	m.Devices, err = u.GetDevices(m.Sites)
	u.LogError(err, "unifi.GetDevices()")
	// Make a new Influx Points Batcher.
	m.BatchPoints, err = influx.NewBatchPoints(influx.BatchPointsConfig{Database: u.InfluxDB})
	u.LogError(err, "influx.NewBatchPoints")
	return m, err
}

// ReportMetrics batches all the metrics and writes them to InfluxDB.
// Returns an error if the write to influx fails.
func (u *UnifiPoller) ReportMetrics(metrics *Metrics) error {
	// Batch (and send) all the points.
	for _, err := range metrics.ProcessPoints() {
		u.LogError(err, "asset.Points()")
	}
	err := u.Write(metrics.BatchPoints)
	if err != nil {
		return errors.Wrap(err, "influxdb.Write(points)")
	}
	var fields, points int
	for _, p := range metrics.Points() {
		points++
		i, _ := p.Fields()
		fields += len(i)
	}
	u.Logf("UniFi Measurements Recorded. Sites: %d, Clients: %d, "+
		"Wireless APs: %d, Gateways: %d, Switches: %d, Points: %d, Fields: %d",
		len(metrics.Sites), len(metrics.Clients), len(metrics.UAPs),
		len(metrics.USGs), len(metrics.USWs), points, fields)
	return nil
}

// ProcessPoints batches all device and client data into influxdb data points.
// Call this after you've collected all the data you care about.
// This function is sorta weird and returns a slice of errors. The reasoning is
// that some points may process while others fail, so we attempt to process them
// all. This is (usually) run in a loop, so we can't really exit on error,
// we just log the errors and tally them on a counter. In reality, this never
// returns any errors because we control the data going in; cool right? But we
// still check&log it in case the data going is skewed up and causes errors!
func (m *Metrics) ProcessPoints() (errs []error) {
	for _, asset := range m.Sites {
		errs = append(errs, m.processPoints(asset))
	}
	for _, asset := range m.Clients {
		errs = append(errs, m.processPoints(asset))
	}
	if m.Devices == nil {
		return
	}
	for _, asset := range m.UAPs {
		errs = append(errs, m.processPoints(asset))
	}
	for _, asset := range m.USGs {
		errs = append(errs, m.processPoints(asset))
	}
	for _, asset := range m.USWs {
		errs = append(errs, m.processPoints(asset))
	}
	return
}

// processPoints is helper function for ProcessPoints.
func (m *Metrics) processPoints(asset Asset) error {
	if asset == nil {
		return nil
	}
	points, err := asset.Points()
	if err != nil || points == nil {
		return err
	}
	m.BatchPoints.AddPoints(points)
	return nil
}

// GetFilteredSites returns a list of sites to fetch data for.
// Omits requested but unconfigured sites. Grabs the full list from the
// controller and returns the sites provided in the config file.
func (u *UnifiPoller) GetFilteredSites() (unifi.Sites, error) {
	sites, err := u.GetSites()
	if err != nil {
		return nil, err
	} else if len(u.Sites) < 1 || StringInSlice("all", u.Sites) {
		return sites, nil
	}
	var i int
	for _, s := range sites {
		// Only include valid sites in the request filter.
		if StringInSlice(s.Name, u.Sites) {
			sites[i] = s
			i++
		}
	}
	return sites[:i], nil
}
