package monitor

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/kube-dc/scseries-csp/internal/sc"
	"github.com/kube-dc/scseries-csp/internal/sc/scfake"
)

func TestPollExportsArrayHealth(t *testing.T) {
	a := scfake.New()
	defer a.Close()
	a.Status = "Degraded"
	ssd := map[string]any{"instanceId": "x", "instanceName": "Read-Intensive SSD"}
	hdd := map[string]any{"instanceId": "y", "instanceName": "10K"}
	a.Disks = []map[string]any{
		{"diskClass": hdd, "healthy": true, "status": "Up"},
		{"diskClass": hdd, "healthy": true, "status": "Up", "spare": true},
		{"diskClass": ssd, "healthy": false, "status": "Up"},
		{"diskClass": ssd, "healthy": false, "status": "Down"},
	}
	a.Alerts = []map[string]any{
		{"status": "Critical", "active": true, "acknowledged": false},
		{"status": "Critical", "active": true, "acknowledged": true},
		{"status": "Degraded", "active": true, "acknowledged": false},
	}
	host, port := a.HostPort()
	c, err := sc.New(context.Background(), sc.Config{Host: host, Port: port, User: a.User, Password: a.Password, InsecureSkipVerify: true},
		slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err != nil {
		t.Fatal(err)
	}
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	if err := m.Poll(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	want := `
# HELP scseries_array_status 1 for the current Storage Center status (Up, Degraded, Down, ...).
# TYPE scseries_array_status gauge
scseries_array_status{array="` + host + `",name="SC-LAB",ssn="64012",status="Degraded"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "scseries_array_status"); err != nil {
		t.Error(err)
	}
	checks := map[string]float64{
		`scseries_array_disks{array="` + host + `",class="Read-Intensive SSD",ssn="64012",state="unhealthy"}`: 2,
		`scseries_array_disks{array="` + host + `",class="10K",ssn="64012",state="spare"}`:                    1,
		`scseries_array_active_alerts{array="` + host + `",severity="Critical",ssn="64012"}`:                  1,
		`scseries_array_active_alerts{array="` + host + `",severity="Degraded",ssn="64012"}`:                  1,
		`scseries_array_up{array="` + host + `"}`:                                                             1,
	}
	got := map[string]float64{}
	mfs, _ := reg.Gather()
	for _, mf := range mfs {
		for _, mt := range mf.GetMetric() {
			var labels []string
			for _, l := range mt.GetLabel() {
				labels = append(labels, l.GetName()+`="`+l.GetValue()+`"`)
			}
			got[mf.GetName()+"{"+strings.Join(labels, ",")+"}"] = mt.GetGauge().GetValue()
		}
	}
	for k, v := range checks {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
	if got[`scseries_array_capacity_bytes{array="`+host+`",kind="free",ssn="64012"}`] != 150373271904256 {
		t.Error("free capacity not exported")
	}
}
