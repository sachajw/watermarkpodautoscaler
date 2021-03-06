// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-2019 Datadog, Inc.

package watermarkpodautoscaler

import (
	"fmt"
	"math"
	"time"

	"github.com/DataDog/watermarkpodautoscaler/pkg/apis/datadoghq/v1alpha1"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	corelisters "k8s.io/client-go/listers/core/v1"
	metricsclient "k8s.io/kubernetes/pkg/controller/podautoscaler/metrics"
)

// ReplicaCalculation is used to compute the scaling recommendation.
type ReplicaCalculation struct {
	replicaCount int32
	utilization  int64
	timestamp    time.Time
}

// ReplicaCalculatorItf interface for ReplicaCalculator
type ReplicaCalculatorItf interface {
	GetExternalMetricReplicas(logger logr.Logger, target *autoscalingv1.Scale, metric v1alpha1.MetricSpec, wpa *v1alpha1.WatermarkPodAutoscaler) (replicaCalculation ReplicaCalculation, err error)
	GetResourceReplicas(logger logr.Logger, target *autoscalingv1.Scale, metric v1alpha1.MetricSpec, wpa *v1alpha1.WatermarkPodAutoscaler) (replicaCalculation ReplicaCalculation, err error)
}

// ReplicaCalculator is responsible for calculation of the number of replicas
// It contains all the needed information
type ReplicaCalculator struct {
	metricsClient metricsclient.MetricsClient
	podLister     corelisters.PodLister
}

// NewReplicaCalculator returns a ReplicaCalculator object reference
func NewReplicaCalculator(metricsClient metricsclient.MetricsClient, podLister corelisters.PodLister) *ReplicaCalculator {
	return &ReplicaCalculator{
		metricsClient: metricsClient,
		podLister:     podLister,
	}
}

// GetExternalMetricReplicas calculates the desired replica count based on a
// target metric value (as a milli-value) for the external metric in the given
// namespace, and the current replica count.
func (c *ReplicaCalculator) GetExternalMetricReplicas(logger logr.Logger, target *autoscalingv1.Scale, metric v1alpha1.MetricSpec, wpa *v1alpha1.WatermarkPodAutoscaler) (ReplicaCalculation, error) {
	lbl, err := labels.Parse(target.Status.Selector)
	if err != nil {
		log.Error(err, "Could not parse the labels of the target")
	}
	currentReadyReplicas, err := c.getReadyPodsCount(metav1.NamespaceAll, lbl, time.Duration(wpa.Spec.ReadinessDelaySeconds)*time.Second)
	if err != nil {
		return ReplicaCalculation{}, fmt.Errorf("unable to get the number of ready pods across all namespaces for %v: %s", lbl, err.Error())
	}
	averaged := 1.0
	if wpa.Spec.Algorithm == "average" {
		averaged = float64(currentReadyReplicas)
	}

	metricName := metric.External.MetricName
	selector := metric.External.MetricSelector
	labelSelector, err := metav1.LabelSelectorAsSelector(selector)
	if err != nil {
		return ReplicaCalculation{}, err
	}

	metrics, timestamp, err := c.metricsClient.GetExternalMetric(metricName, wpa.Namespace, labelSelector)
	if err != nil {
		// When we add official support for several metrics, move this Delete to only occur if no metric is available at all.
		labelsWithReason := prometheus.Labels{
			wpaNamePromLabel:           wpa.Name,
			resourceNamespacePromLabel: wpa.Namespace,
			resourceNamePromLabel:      wpa.Spec.ScaleTargetRef.Name,
			resourceKindPromLabel:      wpa.Spec.ScaleTargetRef.Kind,
			reasonPromLabel:            upscaleCappingPromLabel}
		restrictedScaling.Delete(labelsWithReason)
		labelsWithReason[reasonPromLabel] = downscaleCappingPromLabel
		restrictedScaling.Delete(labelsWithReason)
		labelsWithReason[reasonPromLabel] = "within_bounds"
		restrictedScaling.Delete(labelsWithReason)
		value.Delete(prometheus.Labels{wpaNamePromLabel: wpa.Name, metricNamePromLabel: metricName})
		return ReplicaCalculation{0, 0, time.Time{}}, fmt.Errorf("unable to get external metric %s/%s/%+v: %s", wpa.Namespace, metricName, selector, err)
	}
	logger.V(4).Info("Metrics from the External Metrics Provider", "metrics", metrics)

	var sum int64
	for _, val := range metrics {
		sum += val
	}

	// if the average algorithm is used, the metrics retrieved has to be divided by the number of available replicas.
	adjustedUsage := float64(sum) / averaged
	replicaCount, utilizationQuantity := getReplicaCount(logger, currentReadyReplicas, wpa, metricName, adjustedUsage, metric.External.LowWatermark, metric.External.HighWatermark)
	return ReplicaCalculation{replicaCount, utilizationQuantity, timestamp}, nil
}

// GetResourceReplicas calculates the desired replica count based on a target resource utilization percentage
// of the given resource for pods matching the given selector in the given namespace, and the current replica count
func (c *ReplicaCalculator) GetResourceReplicas(logger logr.Logger, target *autoscalingv1.Scale, metric v1alpha1.MetricSpec, wpa *v1alpha1.WatermarkPodAutoscaler) (ReplicaCalculation, error) {

	resourceName := metric.Resource.Name
	selector := metric.Resource.MetricSelector
	labelSelector, err := metav1.LabelSelectorAsSelector(selector)
	if err != nil {
		return ReplicaCalculation{0, 0, time.Time{}}, err
	}

	namespace := wpa.Namespace
	metrics, timestamp, err := c.metricsClient.GetResourceMetric(resourceName, namespace, labelSelector)
	if err != nil {
		// When we add official support for several metrics, move this Delete to only occur if no metric is available at all.
		labelsWithReason := prometheus.Labels{
			wpaNamePromLabel:           wpa.Name,
			resourceNamespacePromLabel: wpa.Namespace,
			resourceNamePromLabel:      wpa.Spec.ScaleTargetRef.Name,
			resourceKindPromLabel:      wpa.Spec.ScaleTargetRef.Kind,
			reasonPromLabel:            upscaleCappingPromLabel}
		restrictedScaling.Delete(labelsWithReason)
		labelsWithReason[reasonPromLabel] = downscaleCappingPromLabel
		restrictedScaling.Delete(labelsWithReason)
		labelsWithReason[reasonPromLabel] = "within_bounds"
		restrictedScaling.Delete(labelsWithReason)
		value.Delete(prometheus.Labels{wpaNamePromLabel: wpa.Name, metricNamePromLabel: string(resourceName)})
		return ReplicaCalculation{0, 0, time.Time{}}, fmt.Errorf("unable to get resource metric %s/%s/%+v: %s", wpa.Namespace, resourceName, selector, err)
	}
	logger.V(4).Info("Metrics from the Resource Client", "metrics", metrics)

	lbl, err := labels.Parse(target.Status.Selector)
	if err != nil {
		return ReplicaCalculation{0, 0, time.Time{}}, fmt.Errorf("could not parse the labels of the target: %v", err)
	}

	podList, err := c.podLister.Pods(namespace).List(lbl)
	if err != nil {
		return ReplicaCalculation{0, 0, time.Time{}}, fmt.Errorf("unable to get pods while calculating replica count: %v", err)
	}

	if len(podList) == 0 {
		return ReplicaCalculation{0, 0, time.Time{}}, fmt.Errorf("no pods returned by selector while calculating replica count")
	}

	readyPods, ignoredPods := groupPods(logger, podList, metrics, resourceName, time.Duration(wpa.Spec.ReadinessDelaySeconds)*time.Second)
	readyPodCount := len(readyPods)

	removeMetricsForPods(metrics, ignoredPods)
	if len(metrics) == 0 {
		return ReplicaCalculation{0, 0, time.Time{}}, fmt.Errorf("did not receive metrics for any ready pods")
	}

	averaged := 1.0
	if wpa.Spec.Algorithm == "average" {
		averaged = float64(readyPodCount)
	}

	var sum int64
	for _, podMetric := range metrics {
		sum += podMetric.Value
	}
	adjustedUsage := float64(sum) / averaged

	replicaCount, utilizationQuantity := getReplicaCount(logger, target.Status.Replicas, wpa, string(resourceName), adjustedUsage, metric.Resource.LowWatermark, metric.Resource.HighWatermark)
	return ReplicaCalculation{replicaCount, utilizationQuantity, timestamp}, nil
}

func getReplicaCount(logger logr.Logger, currentReplicas int32, wpa *v1alpha1.WatermarkPodAutoscaler, name string, adjustedUsage float64, lowMark, highMark *resource.Quantity) (replicaCount int32, utilization int64) {
	utilizationQuantity := resource.NewMilliQuantity(int64(adjustedUsage), resource.DecimalSI)

	adjustedHM := float64(highMark.MilliValue()) + wpa.Spec.Tolerance*float64(highMark.MilliValue())
	adjustedLM := float64(lowMark.MilliValue()) - wpa.Spec.Tolerance*float64(lowMark.MilliValue())

	labelsWithReason := prometheus.Labels{wpaNamePromLabel: wpa.Name, resourceNamespacePromLabel: wpa.Namespace, resourceNamePromLabel: wpa.Spec.ScaleTargetRef.Name, resourceKindPromLabel: wpa.Spec.ScaleTargetRef.Kind, reasonPromLabel: "within_bounds"}
	labelsWithMetricName := prometheus.Labels{wpaNamePromLabel: wpa.Name, resourceNamespacePromLabel: wpa.Namespace, resourceNamePromLabel: wpa.Spec.ScaleTargetRef.Name, resourceKindPromLabel: wpa.Spec.ScaleTargetRef.Kind, metricNamePromLabel: name}

	switch {
	case adjustedUsage > adjustedHM:
		replicaCount = int32(math.Ceil(float64(currentReplicas) * adjustedUsage / (float64(highMark.MilliValue()))))
		logger.Info("Value is above highMark", "usage", utilizationQuantity.String(), "replicaCount", replicaCount)
	case adjustedUsage < adjustedLM:
		replicaCount = int32(math.Floor(float64(currentReplicas) * adjustedUsage / (float64(lowMark.MilliValue()))))
		// Keep a minimum of 1 replica
		replicaCount = int32(math.Max(float64(replicaCount), 1))
		logger.Info("Value is below lowMark", "usage", utilizationQuantity.String(), "replicaCount", replicaCount)
	default:
		restrictedScaling.With(labelsWithReason).Set(1)
		value.With(labelsWithMetricName).Set(adjustedUsage)
		logger.Info("Within bounds of the watermarks", "value", utilizationQuantity.String(), "lwm", lowMark.String(), "hwm", highMark.String(), "tolerance", wpa.Spec.Tolerance)
		// returning the currentReplicas instead of the count of healthy ones to be consistent with the upstream behavior.
		return currentReplicas, utilizationQuantity.MilliValue()
	}

	restrictedScaling.With(labelsWithReason).Set(0)
	value.With(labelsWithMetricName).Set(adjustedUsage)

	return replicaCount, utilizationQuantity.MilliValue()
}

func (c *ReplicaCalculator) getReadyPodsCount(namespace string, selector labels.Selector, readinessDelay time.Duration) (int32, error) {
	podList, err := c.podLister.Pods(namespace).List(selector)
	if err != nil {
		return 0, fmt.Errorf("unable to get pods while calculating replica count: %v", err)
	}

	if len(podList) == 0 {
		return 0, fmt.Errorf("no pods returned by selector while calculating replica count")
	}

	toleratedAsReadyPodCount := 0

	for _, pod := range podList {
		_, condition := getPodCondition(&pod.Status, corev1.PodReady)
		if condition == nil || pod.Status.StartTime == nil {
			log.V(4).Info("Pod unready", "namespace", pod.Namespace, "name", pod.Name)
			continue
		}
		if pod.Status.Phase == corev1.PodRunning && condition.Status == corev1.ConditionTrue ||
			// We only care about the time after start as a warm up. If a pod becomes unresponsive it will not elect for a readinessDelay tolerance.
			// We do not use v1.ConditionFalse, because we only tolerate the Pending state stuck in the same condition for more than readinessDelay.
			// Pending includes the time spent pulling images onto the host.
			pod.Status.Phase == corev1.PodPending && condition.LastTransitionTime.Sub(pod.Status.StartTime.Time) < readinessDelay {
			toleratedAsReadyPodCount++
		}
	}
	if toleratedAsReadyPodCount == 0 {
		return 0, fmt.Errorf("among the %d pods, none is ready. Skipping recommendation", len(podList))
	}
	return int32(toleratedAsReadyPodCount), nil
}

func groupPods(logger logr.Logger, podList []*corev1.Pod, metrics metricsclient.PodMetricsInfo, resource corev1.ResourceName, delayOfInitialReadinessStatus time.Duration) (readyPods, ignoredPods sets.String) {
	readyPods = sets.NewString()
	ignoredPods = sets.NewString()
	missing := sets.NewString()
	for _, pod := range podList {
		// Failed pods shouldn't produce metrics, but add to ignoredPods to be safe
		if pod.Status.Phase == corev1.PodFailed {
			ignoredPods.Insert(pod.Name)
			continue
		}
		// Pending pods are ignored with Resource metrics.
		if pod.Status.Phase == corev1.PodPending {
			ignoredPods.Insert(pod.Name)
			continue
		}
		// Pods missing metrics
		_, found := metrics[pod.Name]
		if !found {
			missing.Insert(pod.Name)
			continue
		}

		// Unready pods are ignored.
		if resource == corev1.ResourceCPU {
			var ignorePod bool
			_, condition := getPodCondition(&pod.Status, corev1.PodReady)

			if condition == nil || pod.Status.StartTime == nil {
				ignorePod = true
			} else {
				// Ignore metric if pod is unready and it has never been ready.
				ignorePod = condition.Status == corev1.ConditionFalse && pod.Status.StartTime.Add(delayOfInitialReadinessStatus).After(condition.LastTransitionTime.Time)
			}
			if ignorePod {
				ignoredPods.Insert(pod.Name)
				continue
			}
		}
		readyPods.Insert(pod.Name)
	}
	logger.V(2).Info("GroupPods", "Ready", len(readyPods), "Missing", len(missing), "Ignored", len(ignoredPods))
	return readyPods, ignoredPods
}

func removeMetricsForPods(metrics metricsclient.PodMetricsInfo, pods sets.String) {
	for _, pod := range pods.UnsortedList() {
		delete(metrics, pod)
	}
}

// getPodCondition extracts the provided condition from the given status and returns that, and the
// index of the located condition. Returns nil and -1 if the condition is not present.
func getPodCondition(status *corev1.PodStatus, conditionType corev1.PodConditionType) (int, *corev1.PodCondition) {
	if status == nil {
		return -1, nil
	}
	for i := range status.Conditions {
		if status.Conditions[i].Type == conditionType {
			return i, &status.Conditions[i]
		}
	}
	return -1, nil
}
