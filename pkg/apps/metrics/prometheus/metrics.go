package prometheus

import (
	"fmt"
	"strings"
	"time"

	"github.com/golang/glog"
	"github.com/prometheus/client_golang/prometheus"

	"k8s.io/apimachinery/pkg/labels"
	kcorelisters "k8s.io/kubernetes/pkg/client/listers/core/v1"

	"github.com/openshift/origin/pkg/apps/util"
)

const (
	completeRolloutCount         = "complete_rollouts_total"
	activeRolloutDurationSeconds = "active_rollouts_duration_seconds"
	lastFailedRolloutTime        = "last_failed_rollout_time"

	availablePhase = "available"
	failedPhase    = "failed"
	cancelledPhase = "cancelled"
)

var (
	nameToQuery = func(name string) string {
		return strings.Join([]string{"openshift_apps_deploymentconfigs", name}, "_")
	}

	completeRolloutCountDesc = prometheus.NewDesc(
		nameToQuery(completeRolloutCount),
		"Counts total complete rollouts",
		[]string{"phase"}, nil,
	)

	lastFailedRolloutTimeDesc = prometheus.NewDesc(
		nameToQuery(lastFailedRolloutTime),
		"Tracks the time of last failure rollout per deployment config",
		[]string{"namespace", "name", "generation"}, nil,
	)

	activeRolloutDurationSecondsDesc = prometheus.NewDesc(
		nameToQuery(activeRolloutDurationSeconds),
		"Tracks the active rollout duration in seconds",
		[]string{"namespace", "name", "phase", "generation"}, nil,
	)

	apps       = appsCollector{}
	registered = false
)

type appsCollector struct {
	lister kcorelisters.ReplicationControllerLister
}

func IntializeMetricsCollector(rcLister kcorelisters.ReplicationControllerLister) {
	apps.lister = rcLister
	if !registered {
		prometheus.MustRegister(&apps)
		registered = true
	}
	glog.V(4).Info("apps metrics registered with prometheus")
}

func (c *appsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- completeRolloutCountDesc
	ch <- activeRolloutDurationSecondsDesc
}

type failedRollout struct {
	timestamp  float64
	generation int64
}

// Collect implements the prometheus.Collector interface.
func (c *appsCollector) Collect(ch chan<- prometheus.Metric) {
	result, err := c.lister.List(labels.Everything())
	if err != nil {
		glog.V(4).Infof("Collecting metrics for apps failed: %v", err)
		return
	}

	var available, failed, cancelled float64

	latestFailedRollouts := map[string]failedRollout{}

	for _, d := range result {
		dcName := util.DeploymentConfigNameFor(d)
		if len(dcName) == 0 {
			continue
		}

		if util.IsTerminatedDeployment(d) {
			if util.IsDeploymentCancelled(d) {
				cancelled++
				continue
			}
			if util.IsFailedDeployment(d) {
				failed++

				// Track the latest failed rollout per deployment config
				shouldUpdate := false
				if r, exists := latestFailedRollouts[d.Namespace+"/"+dcName]; exists {
					if d.Status.ObservedGeneration > r.generation {
						shouldUpdate = true
					}
				}
				if shouldUpdate {
					latestFailedRollouts[d.Namespace+"/"+dcName] = failedRollout{
						timestamp:  float64(d.CreationTimestamp.Unix()),
						generation: d.Status.ObservedGeneration,
					}
				}
				continue
			}
			if util.IsCompleteDeployment(d) {
				available++
				continue
			}
		}

		// TODO: Figure out under what circumstances the phase is not set.
		phase := strings.ToLower(string(util.DeploymentStatusFor(d)))
		if len(phase) == 0 {
			phase = "unknown"
		}

		// Record duration in seconds for active rollouts
		// TODO: possible time screw?
		durationSeconds := time.Now().Unix() - d.CreationTimestamp.Unix()
		ch <- prometheus.MustNewConstMetric(
			activeRolloutDurationSecondsDesc,
			prometheus.CounterValue,
			float64(durationSeconds),
			[]string{
				d.Namespace,
				dcName,
				phase,
				fmt.Sprintf("%d", d.Status.ObservedGeneration),
			}...)
	}

	// Record latest failed rollouts
	for dc, r := range latestFailedRollouts {
		parts := strings.Split(dc, "/")
		ch <- prometheus.MustNewConstMetric(
			lastFailedRolloutTimeDesc,
			prometheus.GaugeValue,
			r.timestamp,
			[]string{
				parts[0],
				parts[1],
				fmt.Sprintf("%d", r.generation),
			}...)
	}

	ch <- prometheus.MustNewConstMetric(completeRolloutCountDesc, prometheus.GaugeValue, available, []string{availablePhase}...)
	ch <- prometheus.MustNewConstMetric(completeRolloutCountDesc, prometheus.GaugeValue, failed, []string{failedPhase}...)
	ch <- prometheus.MustNewConstMetric(completeRolloutCountDesc, prometheus.GaugeValue, cancelled, []string{cancelledPhase}...)
}
