package render

import (
	"encoding/json"
	"testing"

	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	corev1 "k8s.io/api/core/v1"
)

func TestCSIIncusHelmRepositoryIsOCI(t *testing.T) {
	repo := CSIIncusHelmRepository("default")

	if repo.Name != CSIIncusHelmRepositoryName {
		t.Errorf("name = %q, want %q", repo.Name, CSIIncusHelmRepositoryName)
	}
	if repo.Namespace != "default" {
		t.Errorf("namespace = %q, want default", repo.Namespace)
	}
	if repo.Spec.Type != sourcev1.HelmRepositoryTypeOCI {
		t.Errorf("type = %q, want %q", repo.Spec.Type, sourcev1.HelmRepositoryTypeOCI)
	}
	if repo.Spec.URL != csiIncusChart.Repository {
		t.Errorf("url = %q, want %q", repo.Spec.URL, csiIncusChart.Repository)
	}
}

func TestCSIIncusHelmValuesRemapsKeys(t *testing.T) {
	source := &corev1.Secret{
		Data: map[string][]byte{
			"server":     []byte("https://incus.example.com:8443"),
			"client-crt": []byte("client-cert-bytes"),
			"client-key": []byte("client-key-bytes"),
			"server-crt": []byte("server-cert-bytes"),
			"project":    []byte("benchmark-prod"),
		},
	}

	raw, err := CSIIncusHelmValues(source, nil)
	if err != nil {
		t.Fatalf("CSIIncusHelmValues: %v", err)
	}

	var values map[string]any
	if err := json.Unmarshal(raw.Raw, &values); err != nil {
		t.Fatalf("unmarshal values: %v", err)
	}

	incus, ok := values["incus"].(map[string]any)
	if !ok {
		t.Fatalf("values.incus missing or wrong type: %v", values["incus"])
	}
	if incus["project"] != "benchmark-prod" {
		t.Errorf("incus.project = %v, want benchmark-prod", incus["project"])
	}
	if _, ok := incus["insecureSkipVerify"]; ok {
		t.Error("incus.insecureSkipVerify set without insecure-skip-verify in the Secret")
	}
	if incus["addr"] != "https://incus.example.com:8443" {
		t.Errorf("incus.addr = %v, want https://incus.example.com:8443", incus["addr"])
	}
	tls, ok := incus["tls"].(map[string]any)
	if !ok {
		t.Fatalf("values.incus.tls missing or wrong type: %v", incus["tls"])
	}
	if tls["clientCert"] != "client-cert-bytes" {
		t.Errorf("incus.tls.clientCert = %v, want client-cert-bytes", tls["clientCert"])
	}
	if tls["clientKey"] != "client-key-bytes" {
		t.Errorf("incus.tls.clientKey = %v, want client-key-bytes", tls["clientKey"])
	}
	if tls["serverCert"] != "server-cert-bytes" {
		t.Errorf("incus.tls.serverCert = %v, want server-cert-bytes", tls["serverCert"])
	}

	classes, ok := values["storageClasses"].([]any)
	if !ok || len(classes) != 1 {
		t.Fatalf("values.storageClasses = %v, want a single-element list", values["storageClasses"])
	}
	class, ok := classes[0].(map[string]any)
	if !ok {
		t.Fatalf("storageClasses[0] is not a map: %v", classes[0])
	}
	if class["name"] != CSIIncusStorageClassName {
		t.Errorf("storageClasses[0].name = %v, want %q", class["name"], CSIIncusStorageClassName)
	}
	if class["pool"] != CSIIncusStoragePool {
		t.Errorf("storageClasses[0].pool = %v, want %q", class["pool"], CSIIncusStoragePool)
	}
	if class["contentType"] != "block" {
		t.Errorf("storageClasses[0].contentType = %v, want block", class["contentType"])
	}
	if class["isDefaultClass"] != true {
		t.Errorf("storageClasses[0].isDefaultClass = %v, want true", class["isDefaultClass"])
	}

	snapClass, ok := values["volumeSnapshotClass"].(map[string]any)
	if !ok {
		t.Fatalf("values.volumeSnapshotClass missing or wrong type: %v", values["volumeSnapshotClass"])
	}
	if snapClass["create"] != false {
		t.Errorf("volumeSnapshotClass.create = %v, want false", snapClass["create"])
	}
}

func TestCSIIncusHelmRelease(t *testing.T) {
	values, err := CSIIncusHelmValues(&corev1.Secret{Data: map[string][]byte{}}, nil)
	if err != nil {
		t.Fatalf("CSIIncusHelmValues: %v", err)
	}

	hr := CSIIncusHelmRelease(addonTestCluster("my-cluster"), values)

	if hr.Name != "csi-incus-my-cluster" || hr.Spec.ReleaseName != CSIIncusHelmReleaseName {
		t.Errorf("name = %q / releaseName = %q, want a per-cluster name and releaseName %q", hr.Name, hr.Spec.ReleaseName, CSIIncusHelmReleaseName)
	}
	if hr.Namespace != "default" {
		t.Errorf("namespace = %q, want default", hr.Namespace)
	}
	if hr.Spec.Chart.Spec.Chart != csiIncusChartName {
		t.Errorf("chart = %q, want %q", hr.Spec.Chart.Spec.Chart, csiIncusChartName)
	}
	if hr.Spec.Chart.Spec.SourceRef.Name != CSIIncusHelmRepositoryName {
		t.Errorf("sourceRef.name = %q, want %q", hr.Spec.Chart.Spec.SourceRef.Name, CSIIncusHelmRepositoryName)
	}
	if hr.Spec.TargetNamespace != CSIIncusNamespace {
		t.Errorf("targetNamespace = %q, want %q", hr.Spec.TargetNamespace, CSIIncusNamespace)
	}
	if hr.Spec.KubeConfig == nil || hr.Spec.KubeConfig.SecretRef.Name != "my-cluster-kubeconfig" {
		t.Errorf("kubeConfig.secretRef.name = %v, want my-cluster-kubeconfig", hr.Spec.KubeConfig)
	}
}

func TestCSIIncusHelmValuesOptionalConnectionSettings(t *testing.T) {
	values := func(data map[string][]byte) map[string]any {
		t.Helper()
		raw, err := CSIIncusHelmValues(&corev1.Secret{Data: data}, nil)
		if err != nil {
			t.Fatal(err)
		}
		var v map[string]any
		if err := json.Unmarshal(raw.Raw, &v); err != nil {
			t.Fatal(err)
		}
		return v["incus"].(map[string]any)
	}
	// No project: left to the chart's default (the "default" project).
	if incus := values(map[string][]byte{"server": []byte("https://incus:8443")}); incus["project"] != nil {
		t.Errorf("incus.project = %v, want unset", incus["project"])
	}
	if incus := values(map[string][]byte{"insecure-skip-verify": []byte("true")}); incus["insecureSkipVerify"] != true {
		t.Errorf("incus.insecureSkipVerify = %v, want true", incus["insecureSkipVerify"])
	}
}
