package render

import (
	"encoding/json"
	"testing"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
)

func TestDeschedulerHelmValues(t *testing.T) {
	raw, err := DeschedulerHelmValues(nil)
	if err != nil {
		t.Fatalf("DeschedulerHelmValues: %v", err)
	}
	var values map[string]any
	if err := json.Unmarshal(raw.Raw, &values); err != nil {
		t.Fatalf("unmarshal values: %v", err)
	}
	if values["schedule"] != deschedulerSchedule {
		t.Errorf("schedule = %v, want %q", values["schedule"], deschedulerSchedule)
	}
	if _, ok := values["nodeSelector"]; ok {
		t.Error("nodeSelector set with no monitoring pool - want it left at the chart's own default")
	}
}

func TestDeschedulerHelmValuesTargetsMonitoringPool(t *testing.T) {
	wc := &benchv1alpha1.WorkloadCluster{Spec: benchv1alpha1.WorkloadClusterSpec{
		WorkerPools: []benchv1alpha1.WorkerPoolSpec{{Name: "mon", Role: benchv1alpha1.WorkerPoolRoleMonitoring}},
	}}
	raw, err := DeschedulerHelmValues(wc)
	if err != nil {
		t.Fatalf("DeschedulerHelmValues: %v", err)
	}
	var values map[string]any
	if err := json.Unmarshal(raw.Raw, &values); err != nil {
		t.Fatalf("unmarshal values: %v", err)
	}
	sel, _ := values["nodeSelector"].(map[string]any)
	if sel[NodePoolLabel] != "monitoring" {
		t.Errorf("nodeSelector[%s] = %v, want monitoring", NodePoolLabel, sel[NodePoolLabel])
	}
	if _, ok := values["tolerations"]; !ok {
		t.Error("tolerations not set")
	}
}

func TestDeschedulerHelmRelease(t *testing.T) {
	values, err := DeschedulerHelmValues(nil)
	if err != nil {
		t.Fatalf("DeschedulerHelmValues: %v", err)
	}
	hr := DeschedulerHelmRelease(addonTestCluster("wc-1"), values)
	if hr.Spec.TargetNamespace != DeschedulerNamespace {
		t.Errorf("targetNamespace = %q, want %q", hr.Spec.TargetNamespace, DeschedulerNamespace)
	}
	if hr.Spec.KubeConfig == nil || hr.Spec.KubeConfig.SecretRef.Name != "wc-1-kubeconfig" {
		t.Errorf("kubeConfig = %+v, want secretRef wc-1-kubeconfig", hr.Spec.KubeConfig)
	}
	if hr.Spec.Chart.Spec.Chart != deschedulerChartName || hr.Spec.Chart.Spec.Version != deschedulerChart.Version {
		t.Errorf("chart = %s@%s, want %s@%s", hr.Spec.Chart.Spec.Chart, hr.Spec.Chart.Spec.Version, deschedulerChartName, deschedulerChart.Version)
	}
}
