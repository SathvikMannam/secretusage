package controller

import (
	"fmt"
	"os"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/util/jsonpath"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

// ReferenceRule declares where a custom resource names a Secret. Enumerating every
// operator's Secret fields in Go does not scale, so the set of custom kinds to track
// is configuration rather than code.
type ReferenceRule struct {
	// APIVersion and Kind identify the custom resource, e.g. "cert-manager.io/v1"
	// and "Certificate".
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`

	// Resource is the plural resource name, e.g. "certificates". The controller does
	// not need it; it exists so the Helm chart can render RBAC from the same rule
	// rather than making you restate every kind in a second place.
	Resource string `json:"resource,omitempty"`

	// Paths are JSONPath expressions that evaluate to Secret names in the same
	// namespace as the object, e.g. "{.spec.secretName}" or "{.spec.tls[*].secretName}".
	Paths []string `json:"paths"`
}

// RuleSet is the on-disk format of the rules file.
type RuleSet struct {
	Rules []ReferenceRule `json:"rules"`
}

// CompiledRule is a ReferenceRule with its JSONPath expressions parsed.
type CompiledRule struct {
	GVK   schema.GroupVersionKind
	paths []compiledPath
}

type compiledPath struct {
	expression string
	jsonPath   *jsonpath.JSONPath
}

// LoadRules reads and compiles a rules file. An empty path yields no rules.
func LoadRules(path string) ([]CompiledRule, error) {
	if path == "" {
		return nil, nil
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read rules file %s: %w", path, err)
	}

	var ruleSet RuleSet
	if err := yaml.UnmarshalStrict(raw, &ruleSet); err != nil {
		return nil, fmt.Errorf("parse rules file %s: %w", path, err)
	}
	return CompileRules(ruleSet.Rules)
}

// CompileRules validates rules and parses their JSONPath expressions. It fails on the
// first invalid rule rather than skipping it, so a typo in a rules file surfaces as a
// startup failure instead of a Secret that silently looks unused.
func CompileRules(rules []ReferenceRule) ([]CompiledRule, error) {
	builtIn := builtInGroupVersionKinds()

	compiled := make([]CompiledRule, 0, len(rules))
	seen := make(map[schema.GroupVersionKind]struct{}, len(rules))

	for i, rule := range rules {
		if rule.APIVersion == "" || rule.Kind == "" {
			return nil, fmt.Errorf("rule %d: apiVersion and kind are required", i)
		}
		if len(rule.Paths) == 0 {
			return nil, fmt.Errorf("rule %d (%s %s): at least one path is required", i, rule.APIVersion, rule.Kind)
		}

		groupVersion, err := schema.ParseGroupVersion(rule.APIVersion)
		if err != nil {
			return nil, fmt.Errorf("rule %d: parse apiVersion %q: %w", i, rule.APIVersion, err)
		}
		gvk := groupVersion.WithKind(rule.Kind)

		// Tracking a built-in kind through a rule as well would count every reference
		// twice, so refuse instead of producing inflated counts.
		if _, ok := builtIn[gvk]; ok {
			return nil, fmt.Errorf("rule %d: %s %s is already tracked natively; remove the rule", i, rule.APIVersion, rule.Kind)
		}
		if _, ok := seen[gvk]; ok {
			return nil, fmt.Errorf("rule %d: duplicate rule for %s %s; merge their paths", i, rule.APIVersion, rule.Kind)
		}
		seen[gvk] = struct{}{}

		paths := make([]compiledPath, 0, len(rule.Paths))
		for _, expression := range rule.Paths {
			normalized := normalizeJSONPath(expression)
			parser := jsonpath.New(fmt.Sprintf("%s/%s", rule.Kind, expression))
			// A rule describes an optional field on many objects; most objects will
			// not have most paths, and that is not an error.
			parser.AllowMissingKeys(true)
			if err := parser.Parse(normalized); err != nil {
				return nil, fmt.Errorf("rule %d (%s %s): parse path %q: %w", i, rule.APIVersion, rule.Kind, expression, err)
			}
			paths = append(paths, compiledPath{expression: expression, jsonPath: parser})
		}

		compiled = append(compiled, CompiledRule{GVK: gvk, paths: paths})
	}
	return compiled, nil
}

// normalizeJSONPath accepts both "{.spec.secretName}" and ".spec.secretName", since
// the brace form is a detail of the JSONPath implementation rather than something a
// rules file should have to know about.
func normalizeJSONPath(expression string) string {
	trimmed := strings.TrimSpace(expression)
	if strings.HasPrefix(trimmed, "{") {
		return trimmed
	}
	return "{" + trimmed + "}"
}

// builtInGroupVersionKinds returns the kinds this controller tracks in code.
func builtInGroupVersionKinds() map[schema.GroupVersionKind]struct{} {
	builtIn := make(map[schema.GroupVersionKind]struct{})
	for _, obj := range IndexedObjects() {
		apiVersion, kind := apiVersionKindForObject(obj)
		groupVersion, err := schema.ParseGroupVersion(apiVersion)
		if err != nil {
			continue
		}
		builtIn[groupVersion.WithKind(kind)] = struct{}{}
	}
	return builtIn
}

// Object returns an empty typed placeholder for the rule's kind, for use with the
// field indexer and the watch builder.
func (c CompiledRule) Object() *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(c.GVK)
	return obj
}

// references evaluates every path against obj.
func (c CompiledRule) references(obj *unstructured.Unstructured) []objectSecretReference {
	var refs []objectSecretReference
	for _, path := range c.paths {
		results, err := path.jsonPath.FindResults(obj.Object)
		if err != nil {
			// AllowMissingKeys covers absent fields; anything else means the path does
			// not fit this object's shape, which is per-object and not fatal.
			continue
		}
		for _, group := range results {
			for _, value := range group {
				name, ok := value.Interface().(string)
				if !ok || name == "" {
					continue
				}
				refs = append(refs, objectSecretReference{
					SecretName: name,
					FieldPath:  path.expression,
				})
			}
		}
	}
	return dedupeReferences(refs)
}

// secretNames returns the sorted unique Secret names the rule finds in obj.
func (c CompiledRule) secretNames(obj client.Object) []string {
	unstructuredObj, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return nil
	}
	return uniqueSortedSecretNames(c.references(unstructuredObj))
}
