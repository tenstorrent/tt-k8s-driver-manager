// Command vfio-manage binds Tenstorrent PCI devices to the vfio-pci driver
// so they can be passed through to VMs.
//
// It runs as a privileged DaemonSet next to the controller, re-asserting the
// binding on every tick so a device that drifts off vfio-pci (host reboot,
// driver reload) is recovered without a pod restart, and optionally putting
// the original drivers back on SIGTERM.
package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/tenstorrent/tt-k8s-driver-manager/internal/vfio/binder"
	"github.com/tenstorrent/tt-k8s-driver-manager/internal/vfio/config"
	"github.com/tenstorrent/tt-k8s-driver-manager/internal/vfio/metrics"
)

func main() {
	var (
		configFile    = flag.String("config", "/etc/tt-vfio/config.yaml", "path to YAML config file")
		bindInterval  = flag.Duration("bind-interval", 30*time.Second, "how often to re-assert vfio-pci binding")
		restoreOnExit = flag.Bool("restore-on-exit", false, "attempt to restore original drivers on SIGTERM")
		metricsAddr   = flag.String("metrics-addr", ":9401", "address for the Prometheus /metrics endpoint (empty to disable)")
	)
	flag.Parse()

	log.Print("vfio-manage starting")

	cfg, err := config.Load(*configFile)
	if err != nil {
		log.Fatalf("loading config: %v", err)
	}

	if err := binder.EnsureVFIOPCILoaded(); err != nil {
		log.Fatalf("loading vfio-pci kernel module: %v (ensure /lib/modules is mounted and the container is privileged)", err)
	}

	if binder.NoiommuActive() {
		log.Print("vfio-manage: vfio-noiommu mode detected (enable_unsafe_noiommu_mode=Y)")
		metrics.NoiommuMode.Set(1)
	}

	if *metricsAddr != "" {
		go metrics.Serve(*metricsAddr)
	}

	b := binder.New(cfg, *restoreOnExit)

	done := make(chan struct{})
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-sigs
		log.Printf("vfio-manage: received %v, shutting down", sig)
		close(done)
	}()

	b.RunLoop(*bindInterval, done)

	b.Restore()
	log.Print("vfio-manage: exited")
}
