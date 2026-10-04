package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

type guardMetrics struct {
	lookupActive, lookupLimit prometheus.Gauge
	vendorActive, vendorLimit prometheus.Gauge
	lookupRejected            prometheus.Counter
	vendorRejected            prometheus.Counter
	circuitState              *prometheus.GaugeVec
	circuitRejected           *prometheus.CounterVec
}

// Circuit labels are restricted to these operations; others are ignored.
var circuitOperations = [...]string{"geocoding.search", "forecast.current"}

func (m *Metrics) registerGuards(f promauto.Factory) {
	gauge := func(name, help string) prometheus.Gauge {
		return f.NewGauge(prometheus.GaugeOpts{Name: name, Help: help})
	}
	m.guards = guardMetrics{
		lookupActive: gauge("weatherlookup_lookup_in_flight", "Active admitted weather HTTP requests."),
		lookupLimit:  gauge("weatherlookup_lookup_max_in_flight", "Maximum admitted weather HTTP requests per process."),
		vendorActive: gauge("weatherlookup_vendor_in_flight", "Active vendor-backed lookups including retries and detached fills."),
		vendorLimit:  gauge("weatherlookup_vendor_max_in_flight", "Maximum vendor-backed lookups per process."),
		lookupRejected: f.NewCounter(prometheus.CounterOpts{
			Name: "weatherlookup_lookup_rejected_total",
			Help: "Weather HTTP requests rejected by admission control.",
		}),
		vendorRejected: f.NewCounter(prometheus.CounterOpts{
			Name: "weatherlookup_vendor_admission_rejected_total",
			Help: "Vendor-backed lookups rejected because capacity was exhausted.",
		}),
		circuitState: f.NewGaugeVec(prometheus.GaugeOpts{
			Name: "weatherlookup_circuit_state",
			Help: "Vendor circuit state: 0 closed, 1 open, 2 half-open (one recovery probe).",
		}, []string{"operation"}),
		circuitRejected: f.NewCounterVec(prometheus.CounterOpts{
			Name: "weatherlookup_circuit_rejections_total",
			Help: "Vendor attempts rejected by an open or probing circuit.",
		}, []string{"operation"}),
	}
	// Expose both circuits from startup, so a closed circuit reads 0 rather
	// than being absent.
	for _, operation := range circuitOperations {
		m.guards.circuitState.WithLabelValues(operation)
		m.guards.circuitRejected.WithLabelValues(operation)
	}
}

func (m *Metrics) LookupAdmission(active, maximum int) {
	m.guards.lookupActive.Set(float64(active))
	m.guards.lookupLimit.Set(float64(maximum))
}

func (m *Metrics) LookupRejected() {
	m.guards.lookupRejected.Inc()
}

func (m *Metrics) VendorAdmission(active, maximum int) {
	m.guards.vendorActive.Set(float64(active))
	m.guards.vendorLimit.Set(float64(maximum))
}

func (m *Metrics) VendorRejected() {
	m.guards.vendorRejected.Inc()
}

func (m *Metrics) CircuitState(operation string, state int) {
	if knownCircuit(operation) && state >= 0 && state <= 2 {
		m.guards.circuitState.WithLabelValues(operation).Set(float64(state))
	}
}

func (m *Metrics) CircuitRejected(operation string) {
	if knownCircuit(operation) {
		m.guards.circuitRejected.WithLabelValues(operation).Inc()
	}
}

func knownCircuit(operation string) bool {
	for _, name := range circuitOperations {
		if operation == name {
			return true
		}
	}
	return false
}
