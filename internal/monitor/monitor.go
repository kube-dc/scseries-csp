// Package monitor polls a Storage Center and exports its health as Prometheus metrics, so array problems
// (degraded status, failed disks, missing spares, filling pools, critical alerts) alert like any other
// cluster component.
package monitor

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/kube-dc/scseries-csp/internal/sc"
)

// Metrics are the array health collectors.
type Metrics struct {
	Up       *prometheus.GaugeVec
	Status   *prometheus.GaugeVec
	Capacity *prometheus.GaugeVec
	Disks    *prometheus.GaugeVec
	Alerts   *prometheus.GaugeVec
	LastPoll *prometheus.GaugeVec
}

// NewMetrics registers the collectors on reg.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		Up: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "scseries_array_up",
			Help: "1 if the last poll of the Storage Center REST API succeeded."}, []string{"array"}),
		Status: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "scseries_array_status",
			Help: "1 for the current Storage Center status (Up, Degraded, Down, ...)."}, []string{"array", "ssn", "name", "status"}),
		Capacity: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "scseries_array_capacity_bytes",
			Help: "Storage Center capacity by kind (available, free, used, allocated, configured)."}, []string{"array", "ssn", "kind"}),
		Disks: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "scseries_array_disks",
			Help: "Disks by class and state (healthy, unhealthy, spare)."}, []string{"array", "ssn", "class", "state"}),
		Alerts: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "scseries_array_active_alerts",
			Help: "Active, unacknowledged Storage Center alerts by severity."}, []string{"array", "ssn", "severity"}),
		LastPoll: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "scseries_array_last_poll_timestamp_seconds",
			Help: "Unix time of the last successful poll."}, []string{"array"}),
	}
	reg.MustRegister(m.Up, m.Status, m.Capacity, m.Disks, m.Alerts, m.LastPoll)
	return m
}

type storageCenter struct {
	InstanceID     string      `json:"instanceId"`
	Name           string      `json:"name"`
	ScSerialNumber json.Number `json:"scSerialNumber"`
	Status         string      `json:"status"`
}

type usage struct {
	AvailableSpace  string `json:"availableSpace"`
	FreeSpace       string `json:"freeSpace"`
	UsedSpace       string `json:"usedSpace"`
	AllocatedSpace  string `json:"allocatedSpace"`
	ConfiguredSpace string `json:"configuredSpace"`
}

type disk struct {
	DiskClass *sc.Ref `json:"diskClass"`
	Healthy   bool    `json:"healthy"`
	Status    string  `json:"status"`
	Spare     bool    `json:"spare"`
}

type alert struct {
	Status       string `json:"status"`
	Active       bool   `json:"active"`
	Acknowledged bool   `json:"acknowledged"`
}

// Poll reads the health of the array behind c once and updates the metrics.
func (m *Metrics) Poll(ctx context.Context, c *sc.Client) error {
	array := c.Host()
	var scs []storageCenter
	if err := c.Get(ctx, "StorageCenter/StorageCenter", &scs); err != nil {
		m.Up.WithLabelValues(array).Set(0)
		return err
	}
	ssn := c.SSN()
	var me *storageCenter
	for i := range scs {
		if scs[i].ScSerialNumber.String() == ssn {
			me = &scs[i]
		}
	}
	if me == nil {
		m.Up.WithLabelValues(array).Set(0)
		return nil
	}
	var u usage
	var disks []disk
	var alerts []alert
	for _, call := range []struct {
		path string
		out  any
	}{
		{"StorageCenter/StorageCenter/" + me.InstanceID + "/StorageUsage", &u},
		{"StorageCenter/StorageCenter/" + me.InstanceID + "/DiskList", &disks},
		{"StorageCenter/StorageCenter/" + me.InstanceID + "/ActiveAlertList", &alerts},
	} {
		if err := c.Get(ctx, call.path, call.out); err != nil {
			m.Up.WithLabelValues(array).Set(0)
			return err
		}
	}
	m.Status.DeletePartialMatch(prometheus.Labels{"array": array})
	m.Status.WithLabelValues(array, ssn, me.Name, me.Status).Set(1)
	for kind, v := range map[string]string{"available": u.AvailableSpace, "free": u.FreeSpace, "used": u.UsedSpace,
		"allocated": u.AllocatedSpace, "configured": u.ConfiguredSpace} {
		m.Capacity.WithLabelValues(array, ssn, kind).Set(float64(sc.Bytes(v)))
	}
	m.Disks.DeletePartialMatch(prometheus.Labels{"array": array})
	counts := map[[2]string]int{}
	for _, d := range disks {
		class := "unknown"
		if d.DiskClass != nil && d.DiskClass.InstanceName != "" {
			class = d.DiskClass.InstanceName
		}
		state := "healthy"
		switch {
		case !d.Healthy || d.Status != "Up":
			state = "unhealthy"
		case d.Spare:
			state = "spare"
		}
		counts[[2]string{class, state}]++
	}
	for k, n := range counts {
		m.Disks.WithLabelValues(array, ssn, k[0], k[1]).Set(float64(n))
	}
	m.Alerts.DeletePartialMatch(prometheus.Labels{"array": array})
	sev := map[string]int{"Critical": 0, "Down": 0, "Degraded": 0, "Emergency": 0, "Inform": 0}
	for _, a := range alerts {
		if a.Active && !a.Acknowledged {
			sev[a.Status]++
		}
	}
	for s, n := range sev {
		m.Alerts.WithLabelValues(array, ssn, s).Set(float64(n))
	}
	m.Up.WithLabelValues(array).Set(1)
	m.LastPoll.WithLabelValues(array).Set(float64(time.Now().Unix()))
	return nil
}

// Run polls every interval while active() is true (the leader), until ctx ends.
func (m *Metrics) Run(ctx context.Context, log *slog.Logger, interval time.Duration, connect func(context.Context) (*sc.Client, error), active func() bool) {
	var c *sc.Client
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if active() {
			var err error
			if c == nil {
				c, err = connect(ctx)
			}
			if err == nil {
				pctx, cancel := context.WithTimeout(ctx, interval)
				err = m.Poll(pctx, c)
				cancel()
			}
			if err != nil {
				log.Warn("array health poll failed", "err", err)
			}
		}
		select {
		case <-ctx.Done():
			if c != nil {
				c.Logout(context.Background())
			}
			return
		case <-t.C:
		}
	}
}
