package metrics

import (
	"context"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/theg1239/lecrev/internal/domain"
	"github.com/theg1239/lecrev/internal/store"
)

type StoreCollector struct {
	store store.Store

	buildJobsTotal         *prometheus.Desc
	executionJobsTotal     *prometheus.Desc
	regionState            *prometheus.Desc
	regionAvailableHosts   *prometheus.Desc
	regionBlankWarm        *prometheus.Desc
	regionFunctionWarm     *prometheus.Desc
	regionActiveExecutions *prometheus.Desc
}

func NewHandler(store store.Store) http.Handler {
	registry := prometheus.NewRegistry()
	registry.MustRegister(NewStoreCollector(store))
	return promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
}

func NewStoreCollector(store store.Store) *StoreCollector {
	return &StoreCollector{
		store: store,
		buildJobsTotal: prometheus.NewDesc(
			"lecrev_build_jobs_total",
			"Build jobs grouped by state.",
			[]string{"state"},
			nil,
		),
		executionJobsTotal: prometheus.NewDesc(
			"lecrev_execution_jobs_total",
			"Execution jobs grouped by state.",
			[]string{"state"},
			nil,
		),
		regionState: prometheus.NewDesc(
			"lecrev_region_state",
			"Current state for each configured execution region.",
			[]string{"region", "state"},
			nil,
		),
		regionAvailableHosts: prometheus.NewDesc(
			"lecrev_region_available_hosts",
			"Number of active hosts with available slots in the region.",
			[]string{"region"},
			nil,
		),
		regionBlankWarm: prometheus.NewDesc(
			"lecrev_region_blank_warm",
			"Blank warm snapshot capacity in the region.",
			[]string{"region"},
			nil,
		),
		regionFunctionWarm: prometheus.NewDesc(
			"lecrev_region_function_warm",
			"Function-specific warm snapshot capacity in the region.",
			[]string{"region"},
			nil,
		),
		regionActiveExecutions: prometheus.NewDesc(
			"lecrev_region_active_executions",
			"Non-terminal execution jobs currently targeted at the region.",
			[]string{"region"},
			nil,
		),
	}
}

func (c *StoreCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.buildJobsTotal
	ch <- c.executionJobsTotal
	ch <- c.regionState
	ch <- c.regionAvailableHosts
	ch <- c.regionBlankWarm
	ch <- c.regionFunctionWarm
	ch <- c.regionActiveExecutions
}

func (c *StoreCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	buildStates := []string{"queued", "running", "succeeded", "failed"}
	for _, state := range buildStates {
		count, err := c.store.CountBuildJobsByStates(ctx, []string{state})
		if err != nil {
			continue
		}
		ch <- prometheus.MustNewConstMetric(c.buildJobsTotal, prometheus.GaugeValue, float64(count), state)
	}

	executionStates := []domain.JobState{
		domain.JobStateQueued,
		domain.JobStateScheduling,
		domain.JobStateAssigned,
		domain.JobStateRunning,
		domain.JobStateRetrying,
		domain.JobStateSucceeded,
		domain.JobStateFailed,
	}
	for _, state := range executionStates {
		count, err := c.store.CountExecutionJobsByStates(ctx, []domain.JobState{state})
		if err != nil {
			continue
		}
		ch <- prometheus.MustNewConstMetric(c.executionJobsTotal, prometheus.GaugeValue, float64(count), string(state))
	}

	regions, err := c.store.ListRegions(ctx)
	if err != nil {
		return
	}
	for _, region := range regions {
		ch <- prometheus.MustNewConstMetric(c.regionState, prometheus.GaugeValue, 1, region.Name, region.State)
		ch <- prometheus.MustNewConstMetric(c.regionAvailableHosts, prometheus.GaugeValue, float64(region.AvailableHosts), region.Name)
		ch <- prometheus.MustNewConstMetric(c.regionBlankWarm, prometheus.GaugeValue, float64(region.BlankWarm), region.Name)
		ch <- prometheus.MustNewConstMetric(c.regionFunctionWarm, prometheus.GaugeValue, float64(region.FunctionWarm), region.Name)
		activeExecutions, err := c.store.CountActiveExecutionJobsByRegion(ctx, region.Name)
		if err != nil {
			continue
		}
		ch <- prometheus.MustNewConstMetric(c.regionActiveExecutions, prometheus.GaugeValue, float64(activeExecutions), region.Name)
	}
}
