// Package metrics exposes the Prometheus endpoint for the vfio-manage
// daemon.
//
// This is deliberately separate from internal/metrics: that package
// registers into controller-runtime's registry, which only exists inside
// the controller Deployment. vfio-manage is a plain DaemonSet binary with
// no manager, so it owns a registry and an HTTP server of its own.
package metrics

import (
	"log"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	// DevicesBound tracks PCI devices currently bound to vfio-pci per resource.
	DevicesBound = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "tt_vfio",
		Name:      "devices_bound_total",
		Help:      "Number of PCI devices currently bound to vfio-pci.",
	}, []string{"resource"})

	// BindErrors counts bind or unbind failures, labelled by action ("bind"/"unbind").
	BindErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "tt_vfio",
		Name:      "bind_errors_total",
		Help:      "Cumulative number of vfio-pci bind/unbind failures.",
	}, []string{"action"})

	// NoiommuMode is 1 when the kernel is running in noiommu mode.
	NoiommuMode = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "tt_vfio",
		Name:      "noiommu_mode",
		Help:      "1 if vfio-pci is operating in unsafe-noiommu mode, 0 otherwise.",
	})

	// DeviceInfo is a constant-1 info series carrying each managed device's
	// board identity (n150 vs n300 etc.), which is unreadable from PCI
	// config space once the device is bound to vfio-pci.
	DeviceInfo = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "tt_vfio",
		Name:      "device_info",
		Help:      "Board identity of each vfio-managed device (value is always 1).",
	}, []string{"bdf", "resource", "board_type", "serial"})
)

func init() {
	prometheus.MustRegister(DevicesBound, BindErrors, NoiommuMode, DeviceInfo)
}

// Serve starts the HTTP metrics server on the given address (e.g. ":9401").
// It blocks until the server exits.
func Serve(addr string) {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok")) //nolint:errcheck
	})

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	log.Printf("metrics: listening on %s", addr)
	if err := srv.ListenAndServe(); err != nil {
		log.Printf("metrics: server exited: %v", err)
	}
}
