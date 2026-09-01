package controller

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	usagev1alpha1 "github.com/SathvikMannam/secretusage/api/v1alpha1"
)

var certificateGVK = schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: "Certificate"}

func TestLoadRulesFromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rules.yaml")
	contents := `rules:
  - apiVersion: cert-manager.io/v1
    kind: Certificate
    resource: certificates
    paths:
      - "{.spec.secretName}"
  - apiVersion: external-secrets.io/v1beta1
    kind: ExternalSecret
    resource: externalsecrets
    paths:
      - .spec.target.name
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write rules file: %v", err)
	}

	rules, err := LoadRules(path)
	if err != nil {
		t.Fatalf("load rules: %v", err)
	}
	if len(rules) != 2 {
		t.Fatalf("want 2 rules, got %d", len(rules))
	}
	if rules[0].GVK != certificateGVK {
		t.Fatalf("unexpected GVK: %v", rules[0].GVK)
	}
}

func TestLoadRulesWithEmptyPathIsNotAnError(t *testing.T) {
	rules, err := LoadRules("")
	if err != nil || rules != nil {
		t.Fatalf("want no rules and no error, got %v / %v", rules, err)
	}
}

func TestLoadRulesRejectsUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rules.yaml")
	// A typo such as "path" for "paths" must not silently yield a rule with no paths.
	contents := "rules:\n  - apiVersion: cert-manager.io/v1\n    kind: Certificate\n    path: \"{.spec.secretName}\"\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write rules file: %v", err)
	}
	if _, err := LoadRules(path); err == nil {
		t.Fatal("expected an error for an unknown field")
	}
}

func TestCompileRulesValidation(t *testing.T) {
	cases := []struct {
		name  string
		rules []ReferenceRule
	}{
		{
			name:  "missing kind",
			rules: []ReferenceRule{{APIVersion: "cert-manager.io/v1", Paths: []string{".spec.secretName"}}},
		},
		{
			name:  "no paths",
			rules: []ReferenceRule{{APIVersion: "cert-manager.io/v1", Kind: "Certificate"}},
		},
		{
			name:  "malformed jsonpath",
			rules: []ReferenceRule{{APIVersion: "cert-manager.io/v1", Kind: "Certificate", Paths: []string{"{.spec["}}},
		},
		{
			// Would double count every reference against the native Ingress tracking.
			name:  "kind already tracked natively",
			rules: []ReferenceRule{{APIVersion: "networking.k8s.io/v1", Kind: "Ingress", Paths: []string{".spec.tls[*].secretName"}}},
		},
		{
			name: "duplicate kind",
			rules: []ReferenceRule{
				{APIVersion: "cert-manager.io/v1", Kind: "Certificate", Paths: []string{".spec.secretName"}},
				{APIVersion: "cert-manager.io/v1", Kind: "Certificate", Paths: []string{".spec.other"}},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := CompileRules(tc.rules); err == nil {
				t.Fatal("expected a compile error")
			}
		})
	}
}

func TestCompiledRuleReferences(t *testing.T) {
	rules, err := CompileRules([]ReferenceRule{{
		APIVersion: "example.com/v1",
		Kind:       "Widget",
		Paths: []string{
			"{.spec.secretName}",      // brace form
			".spec.tls[*].secretName", // bare form, multiple matches
			".spec.missing.deeply",    // absent on this object
		},
	}})
	if err != nil {
		t.Fatalf("compile rules: %v", err)
	}

	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "example.com/v1",
		"kind":       "Widget",
		"metadata":   map[string]interface{}{"name": "w", "namespace": "default"},
		"spec": map[string]interface{}{
			"secretName": "primary",
			"tls": []interface{}{
				map[string]interface{}{"secretName": "tls-a"},
				map[string]interface{}{"secretName": "tls-b"},
				map[string]interface{}{"hosts": []interface{}{"x"}}, // no secretName
			},
		},
	}}

	assertStringSliceEqual(t, rules[0].secretNames(obj), []string{"primary", "tls-a", "tls-b"})

	refs := rules[0].references(obj)
	for _, ref := range refs {
		if ref.SecretName == "primary" && ref.FieldPath != "{.spec.secretName}" {
			t.Fatalf("fieldPath should record the configured expression, got %q", ref.FieldPath)
		}
	}
}

func TestCompiledRuleIgnoresNonStringValues(t *testing.T) {
	rules, err := CompileRules([]ReferenceRule{{
		APIVersion: "example.com/v1",
		Kind:       "Widget",
		Paths:      []string{".spec.secretName"},
	}})
	if err != nil {
		t.Fatalf("compile rules: %v", err)
	}

	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"spec": map[string]interface{}{"secretName": int64(42)},
	}}
	if names := rules[0].secretNames(obj); names != nil {
		t.Fatalf("a non-string value must not produce a reference, got %v", names)
	}
}

func TestReconcileTracksCustomResourceViaRule(t *testing.T) {
	ctx := context.Background()
	rules, err := CompileRules([]ReferenceRule{{
		APIVersion: "cert-manager.io/v1",
		Kind:       "Certificate",
		Resource:   "certificates",
		Paths:      []string{".spec.secretName"},
	}})
	if err != nil {
		t.Fatalf("compile rules: %v", err)
	}

	certificate := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "cert-manager.io/v1",
		"kind":       "Certificate",
		"metadata":   map[string]interface{}{"name": "web", "namespace": "default"},
		"spec":       map[string]interface{}{"secretName": "web-tls"},
	}}

	reconciler := newFakeReconcilerWithRules(t, rules, certificate)

	if _, err := reconciler.Reconcile(ctx, request("default", "web-tls")); err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	var usage usagev1alpha1.SecretUsage
	if err := reconciler.Get(ctx, types.NamespacedName{Namespace: "default", Name: "web-tls"}, &usage); err != nil {
		t.Fatalf("expected the Certificate reference to be tracked: %v", err)
	}
	if usage.Status.UsageCount != 1 {
		t.Fatalf("want usageCount=1, got %d", usage.Status.UsageCount)
	}
	got := usage.Status.Usages[0]
	if got.Kind != "Certificate" || got.APIVersion != "cert-manager.io/v1" {
		t.Fatalf("unexpected group/kind: %#v", got)
	}
	if got.Name != "web" || got.FieldPath != ".spec.secretName" {
		t.Fatalf("unexpected reference: %#v", got)
	}
}
