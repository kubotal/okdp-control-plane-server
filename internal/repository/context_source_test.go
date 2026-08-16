package repository

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// twoContexts builds the console and platform Contexts, both empty.
func twoContexts(t *testing.T) *dynamicfake.FakeDynamicClient {
	t.Helper()
	scheme := runtime.NewScheme()
	listKinds := map[schema.GroupVersionResource]string{contextGVR: "ContextList"}

	empty := func(name string) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "kubocd.kubotal.io/v1alpha1",
			"kind":       "Context",
			"metadata":   map[string]interface{}{"name": name, "namespace": "kubocd-system"},
			"spec":       map[string]interface{}{"context": map[string]interface{}{}},
		}}
	}

	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds,
		empty("okdp-control-plane"), empty("platform"))
}

func TestPlatformKeysNameThePlatformContext(t *testing.T) {
	repo := NewContextRepository(twoContexts(t),
		"okdp-control-plane", "kubocd-system", "platform", "kubocd-system")

	cases := []struct {
		name string
		call func() error
	}{
		{"ingress.suffix", func() error {
			_, err := repo.GetIngressSuffix(context.Background())
			return err
		}},
		{"sparkOperator", func() error {
			_, err := repo.GetSparkConfig(context.Background())
			return err
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if err == nil {
				t.Fatal("expected an error naming the Context")
			}
			if !strings.Contains(err.Error(), "kubocd-system/platform") {
				t.Errorf("expected the platform Context to be named, got %q", err.Error())
			}
		})
	}
}

func TestConsoleKeyNamesTheConsoleContext(t *testing.T) {
	repo := NewContextRepository(twoContexts(t),
		"okdp-control-plane", "kubocd-system", "platform", "kubocd-system")

	_, err := repo.GetPackageRepository(context.Background())
	if err == nil {
		t.Fatal("expected an error naming the Context")
	}
	if !strings.Contains(err.Error(), "kubocd-system/okdp-control-plane") {
		t.Errorf("expected the console Context to be named, got %q", err.Error())
	}
}
