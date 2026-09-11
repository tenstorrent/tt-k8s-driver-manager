package metrics

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Serve blocks, so run it against a pre-chosen free port and poll until it
// answers. Verifies the health endpoint and that the vfio gauges registered
// in init() actually come out of /metrics.
func TestServe_HealthAndMetrics(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()

	go Serve(addr)

	var resp *http.Response
	for i := 0; i < 50; i++ {
		resp, err = http.Get(fmt.Sprintf("http://%s/healthz", addr))
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("server never came up on %s: %v", addr, err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Errorf("/healthz = %d %q; want 200 ok", resp.StatusCode, body)
	}

	NoiommuMode.Set(1)
	resp, err = http.Get(fmt.Sprintf("http://%s/metrics", addr))
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "tt_vfio_noiommu_mode 1") {
		t.Error("/metrics does not expose tt_vfio_noiommu_mode")
	}
}
